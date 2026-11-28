package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cestbon0309/crypta/core/crypt"
	"github.com/cestbon0309/crypta/core/internal/fakedrive"
	"github.com/cestbon0309/crypta/core/remote"
)

// An upload of a picture or a video also stores a thumbnail, which is found
// again after the file is renamed, and a file that is neither has none.
func TestThumbnails(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	srv := fakedrive.New()
	defer srv.Close()
	cfg := &Config{Backend: "drive", Path: "enc", Crypt: crypt.Options{Password: "pw"},
		Drive: remote.DriveConfig{ClientID: "id", Token: json.RawMessage(`{"refresh_token":"rt"}`), APIBase: srv.URL, TokenURL: srv.URL + "/token"}}
	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	os.MkdirAll(files, 0o755)
	// The first third of the video is black, the rest is red.
	for _, args := range [][]string{
		{"-f", "lavfi", "-i", "color=black:s=640x360:d=3", "-f", "lavfi", "-i", "color=red:s=640x360:d=7", "-filter_complex", "[0][1]concat=n=2", "-pix_fmt", "yuv420p", "clip.mp4"},
		{"-f", "lavfi", "-i", "color=blue:s=1200x800", "-frames:v", "1", "photo.png"},
		{"-f", "lavfi", "-i", "color=green:s=100x50", "-frames:v", "1", "small.jpg"},
	} {
		cmd := exec.Command(ffmpeg, append([]string{"-v", "error"}, args...)...)
		cmd.Dir = files
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	os.WriteFile(filepath.Join(files, "notes.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(files, "broken.mp4"), []byte("not a video"), 0o644)

	fs, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fs.ChunkSize = 4 * 1024 // the video is stored in chunks
	m, err := NewManager(filepath.Join(dir, "state"), files)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.SetFS(fs)
	go m.Run(ctx)
	for _, name := range []string{"clip.mp4", "photo.png", "small.jpg", "notes.txt", "broken.mp4"} {
		tr, err := m.Add(Transfer{Kind: KindUpload, Local: name, Remote: "media", DeleteSource: true})
		if err != nil {
			t.Fatal(err)
		}
		waitState(t, m, tr.ID, StateDone)
	}
	thumb := func(p string) (w, h int, r, g, b uint32) {
		t.Helper()
		it, err := fs.Stat(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		th, err := fs.Thumb(ctx, it)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		rc, err := fs.Open(ctx, th, 0, -1)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		data, _ := io.ReadAll(rc)
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		t.Logf("%s: %s, %d bytes, %v", p, th.Name, len(data), img.Bounds().Size())
		r, g, b, _ = img.At(img.Bounds().Dx()/2, img.Bounds().Dy()/2).RGBA()
		return img.Bounds().Dx(), img.Bounds().Dy(), r >> 8, g >> 8, b >> 8
	}
	if it, _ := fs.Stat(ctx, "media/clip.mp4"); it == nil || len(it.parts) < 2 {
		t.Fatalf("the video is not chunked: %+v", it)
	}
	if w, h, r, g, _ := thumb("media/clip.mp4"); w != 320 || h != 180 || r < 150 || g > 80 {
		t.Fatalf("video thumbnail %dx%d, red %d green %d", w, h, r, g)
	}
	if w, h, _, _, b := thumb("media/photo.png"); w != 320 || h != 213 || b < 150 {
		t.Fatalf("picture thumbnail %dx%d, blue %d", w, h, b)
	}
	if w, h, _, _, _ := thumb("media/small.jpg"); w != 100 || h != 50 {
		t.Fatalf("a small picture became %dx%d", w, h)
	}
	for _, p := range []string{"media/notes.txt", "media/broken.mp4"} {
		it, err := fs.Stat(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fs.Thumb(ctx, it); !errors.Is(err, remote.ErrNotFound) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if err := fs.Move(ctx, "media/photo.png", "moved/renamed.png"); err != nil {
		t.Fatal(err)
	}
	thumb("moved/renamed.png")
	if list, _ := fs.List(ctx, ThumbDir, false); len(list) != 3 {
		t.Fatalf("%d thumbnails stored", len(list))
	}
}

// Without any thumbnail stored, asking for one is "not found".
func TestNoThumbDir(t *testing.T) {
	srv := fakedrive.New()
	defer srv.Close()
	cfg := &Config{Backend: "drive", Path: "enc", Crypt: crypt.Options{Password: "pw"},
		Drive: remote.DriveConfig{ClientID: "id", Token: json.RawMessage(`{"refresh_token":"rt"}`), APIBase: srv.URL, TokenURL: srv.URL + "/token"}}
	fs, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Thumb(context.Background(), &Item{id: "x"}); !errors.Is(err, remote.ErrNotFound) {
		t.Fatal(err)
	}
}

// A job paused while its thumbnail is made is done: the file is stored.
func TestPauseDuringThumbnail(t *testing.T) {
	bin := t.TempDir()
	started := filepath.Join(bin, "started")
	os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte("#!/bin/sh\ntouch "+started+"\nsleep 30\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	srv := fakedrive.New()
	defer srv.Close()
	cfg := &Config{Backend: "drive", Path: "enc", Crypt: crypt.Options{Password: "pw"},
		Drive: remote.DriveConfig{ClientID: "id", Token: json.RawMessage(`{"refresh_token":"rt"}`), APIBase: srv.URL, TokenURL: srv.URL + "/token"}}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.png"), []byte("picture"), 0o644)
	fs, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(filepath.Join(dir, "state"), dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.SetFS(fs)
	go m.Run(ctx)
	tr, err := m.Add(Transfer{Kind: KindUpload, Local: "a.png", Remote: "", DeleteSource: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if i > 500 {
			t.Fatal("ffmpeg was not started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := m.Act(tr.ID, "pause"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, tr.ID, StateDone)
	if _, err := os.Stat(filepath.Join(dir, "a.png")); !os.IsNotExist(err) {
		t.Fatalf("the source is still there: %v", err)
	}
	if list, _ := fs.List(ctx, "", true); len(list) != 1 || list[0].Name != "a.png" {
		t.Fatalf("stored: %+v", list)
	}
}

// counting counts the bytes read from the remote.
type counting struct {
	remote.Remote
	read atomic.Int64
}

type countedBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (c countedBody) Read(b []byte) (int, error) {
	n, err := c.ReadCloser.Read(b)
	c.n.Add(int64(n))
	return n, err
}

func (c *counting) Open(ctx context.Context, id string, offset, length int64) (io.ReadCloser, error) {
	rc, err := c.Remote.Open(ctx, id, offset, length)
	if err != nil {
		return nil, err
	}
	return countedBody{rc, &c.read}, nil
}

// Files stored without a thumbnail get one when it is asked for, and of a
// video only a part is read for it.
func TestRequestThumbs(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	srv := fakedrive.New()
	defer srv.Close()
	cfg := &Config{Backend: "drive", Path: "enc", Crypt: crypt.Options{Password: "pw"},
		Drive: remote.DriveConfig{ClientID: "id", Token: json.RawMessage(`{"refresh_token":"rt"}`), APIBase: srv.URL, TokenURL: srv.URL + "/token"}}
	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	os.MkdirAll(files, 0o755)
	for _, args := range [][]string{
		// Black for the first tenth, then a moving pattern; the index is at the end of the file.
		{"-f", "lavfi", "-i", "color=black:s=1280x720:d=6", "-f", "lavfi", "-i", "testsrc2=s=1280x720:d=54", "-filter_complex", "[0][1]concat=n=2", "-pix_fmt", "yuv420p", "-b:v", "3M", "long.mp4"},
		{"-f", "lavfi", "-i", "color=blue:s=1200x800", "-frames:v", "1", "photo.png"},
	} {
		cmd := exec.Command(ffmpeg, append([]string{"-v", "error"}, args...)...)
		cmd.Dir = files
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	os.WriteFile(filepath.Join(files, "broken.mp4"), []byte("not a video"), 0o644)
	os.WriteFile(filepath.Join(files, "notes.txt"), []byte("hello"), 0o644)
	fs, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fs.ChunkSize = 1024 * 1024 // the video is stored in chunks
	count := &counting{Remote: fs.R}
	fs.R = count
	m, err := NewManager(filepath.Join(dir, "state"), files)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.SetFS(fs)
	go m.Run(ctx)
	// Stored as a phone stores them: no ffmpeg there.
	path := os.Getenv("PATH")
	os.Setenv("PATH", t.TempDir())
	defer os.Setenv("PATH", path)
	names := []string{"long.mp4", "photo.png", "broken.mp4", "notes.txt"}
	for _, name := range names {
		tr, err := m.Add(Transfer{Kind: KindUpload, Local: name, Remote: "media"})
		if err != nil {
			t.Fatal(err)
		}
		waitState(t, m, tr.ID, StateDone)
	}
	all := []string{"media/long.mp4", "/media/photo.png", "media/broken.mp4", "media/notes.txt", "media/none.mp4", "media"}
	if n := m.RequestThumbs(ctx, fs, all); n != 0 {
		t.Fatalf("%d queued without ffmpeg", n)
	}
	os.Setenv("PATH", path)
	video, _ := fs.Stat(ctx, "media/long.mp4")
	if video == nil || len(video.parts) < 3 {
		t.Fatalf("the video is not chunked: %+v", video)
	}
	count.read.Store(0)
	if n := m.RequestThumbs(ctx, fs, all); n != 3 {
		t.Fatalf("%d queued", n)
	}
	if n := m.RequestThumbs(ctx, fs, all); n != 0 {
		t.Fatalf("%d queued twice", n)
	}
	idle := func() {
		t.Helper()
		for i := 0; ; i++ {
			m.thumbMu.Lock()
			busy := m.thumbBusy
			m.thumbMu.Unlock()
			if !busy {
				return
			}
			if i > 3000 {
				t.Fatal("thumbnails are still being made")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	idle()
	read := count.read.Load()
	t.Logf("read %d of %d bytes", read, video.Size)
	if read > video.Size/2 {
		t.Fatalf("read %d bytes for the thumbnail of a %d byte video", read, video.Size)
	}
	look := func(p string) (w, h int, r, g, b uint32) {
		t.Helper()
		it, err := fs.Stat(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		th, err := fs.Thumb(ctx, it)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		rc, err := fs.Open(ctx, th, 0, -1)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		img, err := jpeg.Decode(rc)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		r, g, b, _ = img.At(10, 10).RGBA()
		return img.Bounds().Dx(), img.Bounds().Dy(), r >> 8, g >> 8, b >> 8
	}
	if w, h, r, g, b := look("media/long.mp4"); w != 320 || h != 180 || r+g+b < 60 {
		t.Fatalf("video thumbnail %dx%d, colour %d %d %d", w, h, r, g, b)
	}
	if w, h, _, _, b := look("media/photo.png"); w != 320 || h != 213 || b < 150 {
		t.Fatalf("picture thumbnail %dx%d, blue %d", w, h, b)
	}
	// The broken video gave nothing and is not tried again; the rest have theirs.
	if n := m.RequestThumbs(ctx, fs, all); n != 0 {
		t.Fatalf("%d queued again", n)
	}
	if list, _ := fs.List(ctx, ThumbDir, true); len(list) != 2 {
		t.Fatalf("%d thumbnails stored", len(list))
	}
}
