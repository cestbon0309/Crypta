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
	"testing"

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
