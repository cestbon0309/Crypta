package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cestbon0309/crypta/core/crypt"
	"github.com/cestbon0309/crypta/core/internal/fakedrive"
	"github.com/cestbon0309/crypta/core/remote"
)

func waitState(t *testing.T, m *Manager, id string, states ...string) Transfer {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, tr := range m.List() {
			if tr.ID == id {
				for _, s := range states {
					if tr.State == s {
						return tr
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("transfer never reached %v: %+v", states, m.List())
	return Transfer{}
}

// An upload that dies half way continues from the byte Drive last stored,
// in a new process (new Manager), and the result decrypts correctly.
func TestDriveResume(t *testing.T) {
	srv := fakedrive.New()
	defer srv.Close()
	cfg := &Config{Backend: "drive", Path: "enc/data", Crypt: crypt.Options{Password: "pw"},
		Drive: remote.DriveConfig{ClientID: "id", Token: json.RawMessage(`{"refresh_token":"rt"}`), APIBase: srv.URL, TokenURL: srv.URL + "/token"}}
	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	os.MkdirAll(files, 0o755)
	// Small chunks, so that the file takes several.
	defer func(n int64) { uploadChunk = n }(uploadChunk)
	uploadChunk = 8 * 1024 * 1024
	data := randBytes(30*1024*1024 + 999)
	os.WriteFile(filepath.Join(files, "movie.mkv"), data, 0o644)

	start := func() (*Manager, *FS, context.CancelFunc) {
		fs, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		m, err := NewManager(filepath.Join(dir, "state"), files)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		m.SetFS(fs)
		go m.Run(ctx)
		return m, fs, cancel
	}

	// First run: Drive rejects a chunk after storing half of it; the job
	// asks Drive for the stored offset and carries on.
	srv.FailAfter = 12 * 1024 * 1024
	m, _, cancel := start()
	m.RetryDelay = time.Millisecond
	tr, err := m.Add(Transfer{Kind: KindUpload, Local: "movie.mkv", Remote: "videos"})
	if err != nil {
		t.Fatal(err)
	}
	// "Kill the process" somewhere in the middle of the upload.
	for {
		if s := waitState(t, m, tr.ID, StateRunning, StateDone); s.State == StateDone {
			t.Fatal("upload finished before it could be interrupted")
		} else if srv.Accepted() > 16*1024*1024 { // stored, not merely on its way
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)

	// Second run: a fresh Manager reads the queue from disk and resumes.
	stored := srv.Accepted()
	m, fs, cancel := start()
	defer cancel()
	waitState(t, m, tr.ID, StateDone)
	if sent := srv.Accepted() - stored; sent > 16*1024*1024 {
		t.Fatalf("resume re-sent %d bytes", sent)
	}
	ctx := context.Background()
	it, err := fs.Stat(ctx, "videos/movie.mkv")
	if err != nil || it.Size != int64(len(data)) {
		t.Fatalf("stat: %v %+v", err, it)
	}
	rc, err := fs.Open(ctx, it, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("resumed upload is corrupt")
	}
	rc, _ = fs.Open(ctx, it, 20_000_000, 1000)
	got, _ = io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data[20_000_000:20_001_000]) {
		t.Fatal("range read differs")
	}

	// Download: interrupt, then resume with the pieces that are there.
	defer func(n int64) { downloadPiece = n }(downloadPiece)
	downloadPiece = 1 << 20
	dl, _ := m.Add(Transfer{Kind: KindDownload, Remote: "videos/movie.mkv", Local: "out/movie.mkv"})
	for {
		if s := waitState(t, m, dl.ID, StateRunning, StateDone, StateQueued); s.State == StateDone {
			t.Fatal("download finished before it could be interrupted")
		} else if s.Done > 8<<20 {
			break
		}
		time.Sleep(100 * time.Microsecond)
	}
	m.Act(dl.ID, "pause")
	waitState(t, m, dl.ID, StatePaused)
	time.Sleep(100 * time.Millisecond)
	served := srv.Served()
	m.Act(dl.ID, "resume")
	waitState(t, m, dl.ID, StateDone)
	if got, _ := os.ReadFile(filepath.Join(files, "out/movie.mkv")); !bytes.Equal(got, data) {
		t.Fatal("download differs")
	}
	if srv.Streams < 2 {
		t.Fatalf("download ran %d stream(s) at a time", srv.Streams)
	}
	if n := srv.Served() - served; n > int64(len(data))-4<<20 {
		t.Fatalf("resumed download fetched %d bytes of %d", n, len(data))
	}

	// A second upload of the name is kept beside the first; rename, delete on Drive.
	os.WriteFile(filepath.Join(files, "movie.mkv"), []byte("v2"), 0o644)
	tr2, _ := m.Add(Transfer{Kind: KindUpload, Local: "movie.mkv", Remote: "videos"})
	if s := waitState(t, m, tr2.ID, StateDone); s.Name != "movie (2).mkv" {
		t.Fatalf("second upload is named %q", s.Name)
	}
	items, _ := fs.List(ctx, "videos", true)
	if len(items) != 2 {
		t.Fatalf("same name: %+v", items)
	}
	if err := fs.Delete(ctx, "videos/movie (2).mkv"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Move(ctx, "videos/movie.mkv", "other/m.mkv"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(ctx, "videos"); err != nil {
		t.Fatal(err)
	}
	items, _ = fs.List(ctx, "", true)
	if len(items) != 1 || items[0].Name != "other" {
		t.Fatalf("root: %+v", items)
	}
}

// A chunked upload that dies half way keeps the chunks that are stored and
// goes on inside the ones that are not; a canceled one leaves nothing.
func TestDriveChunkedResume(t *testing.T) {
	srv := fakedrive.New()
	defer srv.Close()
	cfg := &Config{Backend: "drive", Path: "enc", Crypt: crypt.Options{Password: "pw"}, ChunkSize: 4 << 20,
		Drive: remote.DriveConfig{ClientID: "id", Token: json.RawMessage(`{"refresh_token":"rt"}`), APIBase: srv.URL, TokenURL: srv.URL + "/token"}}
	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	os.MkdirAll(files, 0o755)
	defer func(n int64) { partRequest = n }(partRequest)
	partRequest = 1 << 20
	data := randBytes(30<<20 + 999)
	os.WriteFile(filepath.Join(files, "movie.mkv"), data, 0o644)
	start := func() (*Manager, *FS, context.CancelFunc) {
		fs, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		m, err := NewManager(filepath.Join(dir, "state"), files)
		if err != nil {
			t.Fatal(err)
		}
		m.SetParallel(3)
		ctx, cancel := context.WithCancel(context.Background())
		m.SetFS(fs)
		go m.Run(ctx)
		return m, fs, cancel
	}
	interrupt := func(m *Manager, id string) {
		for {
			if s := waitState(t, m, id, StateRunning, StateDone); s.State == StateDone {
				t.Fatal("upload finished before it could be interrupted")
			} else if s.Done > 12<<20 {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	m, _, cancel := start()
	tr, err := m.Add(Transfer{Kind: KindUpload, Local: "movie.mkv", Remote: "videos"})
	if err != nil {
		t.Fatal(err)
	}
	interrupt(m, tr.ID)
	cancel()
	time.Sleep(50 * time.Millisecond)

	stored := srv.Accepted()
	m, fs, cancel := start()
	defer cancel()
	ctx := context.Background()
	if items, _ := fs.List(ctx, "videos", true); len(items) != 0 {
		t.Fatalf("unfinished chunked upload is listed: %+v", items)
	}
	waitState(t, m, tr.ID, StateDone)
	if sent := srv.Accepted() - stored; sent > int64(len(data))-8<<20 {
		t.Fatalf("resume re-sent %d bytes", sent)
	}
	it, err := fs.Stat(ctx, "videos/movie.mkv")
	if err != nil || it.Size != int64(len(data)) || len(it.parts) != 8 {
		t.Fatalf("stat: %v %+v", err, it)
	}
	rc, err := fs.Open(ctx, it, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("resumed chunked upload is corrupt")
	}

	// Paused, its folder renamed, resumed: the file is whole in the folder
	// under its new name.
	tr3, _ := m.Add(Transfer{Kind: KindUpload, Local: "movie.mkv", Remote: "third"})
	interrupt(m, tr3.ID)
	m.Act(tr3.ID, "pause")
	time.Sleep(100 * time.Millisecond)
	if err := fs.Move(ctx, "third", "fourth"); err != nil {
		t.Fatal(err)
	}
	m.Act(tr3.ID, "resume")
	waitState(t, m, tr3.ID, StateDone)
	if _, err := fs.Stat(ctx, "third"); err == nil {
		t.Fatal("the renamed folder came back")
	}
	if it, err = fs.Stat(ctx, "fourth/movie.mkv"); err != nil || it.Size != int64(len(data)) {
		t.Fatalf("after the rename: %v %+v", err, it)
	}
	rc, _ = fs.Open(ctx, it, 0, -1)
	got, _ = io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("upload resumed after a folder rename is corrupt")
	}

	// Canceled half way: the chunks go, the name stays free.
	tr2, _ := m.Add(Transfer{Kind: KindUpload, Local: "movie.mkv", Remote: "other"})
	interrupt(m, tr2.ID)
	m.Act(tr2.ID, "cancel")
	dc, err := fs.dir(ctx, "other", false)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, err := fs.R.List(ctx, dc.id)
		if err == nil && len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("canceled upload left %d objects (%v)", len(entries), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A download of a chunked file crosses the chunks in every segment, and a
// part file left by a version that wrote front to back is continued.
func TestSegmentedDownload(t *testing.T) {
	srv := fakedrive.New()
	defer srv.Close()
	fs, err := New(&Config{Backend: "drive", Path: "enc", Crypt: crypt.Options{Password: "pw"}, ChunkSize: 3<<20 + 4096,
		Drive: remote.DriveConfig{ClientID: "id", Token: json.RawMessage(`{"refresh_token":"rt"}`), APIBase: srv.URL, TokenURL: srv.URL + "/token"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	os.MkdirAll(files, 0o755)
	m, err := NewManager(filepath.Join(dir, "state"), files)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.SetFS(fs)
	go m.Run(ctx)
	defer func(n int64) { downloadPiece = n }(downloadPiece)
	downloadPiece = 1 << 20
	data := randBytes(20<<20 + 12345)
	os.WriteFile(filepath.Join(files, "big.bin"), data, 0o644)
	os.WriteFile(filepath.Join(files, "empty.bin"), nil, 0o644)
	for _, name := range []string{"big.bin", "empty.bin"} {
		up, _ := m.Add(Transfer{Kind: KindUpload, Local: name, Remote: "d"})
		waitState(t, m, up.ID, StateDone)
	}
	for _, n := range []int{1, 4, 16} {
		m.SetSegments(n)
		srv.Streams = 0
		dl, _ := m.Add(Transfer{Kind: KindDownload, Remote: "d/big.bin", Local: "out/big.bin"})
		s := waitState(t, m, dl.ID, StateDone, StateFailed)
		if got, _ := os.ReadFile(filepath.Join(files, s.Local)); s.State != StateDone || !bytes.Equal(got, data) {
			t.Fatalf("%d segments: %s %s", n, s.State, s.Error)
		}
		if srv.Streams > n || (n > 1 && srv.Streams < 2) {
			t.Fatalf("%d segments ran %d streams at a time", n, srv.Streams)
		}
	}
	dl, _ := m.Add(Transfer{Kind: KindDownload, Remote: "d/empty.bin", Local: "out/empty.bin"})
	waitState(t, m, dl.ID, StateDone)
	if fi, err := os.Stat(filepath.Join(files, "out/empty.bin")); err != nil || fi.Size() != 0 {
		t.Fatal(fi, err)
	}

	// The old part file holds 5 MiB and a bit; the rest is fetched.
	it, _ := fs.Stat(ctx, "d/big.bin")
	os.WriteFile(filepath.Join(files, "out/old.bin.part"), data[:5<<20+777], 0o644)
	m.mu.Lock()
	old := &Transfer{ID: "old", Kind: KindDownload, State: StateQueued, Remote: "d/big.bin", Local: "out/old.bin", Name: "old.bin",
		Size: it.Size, SrcMod: it.ModTime, CreatedAt: time.Now()}
	m.items[old.ID] = old
	m.mu.Unlock()
	served := srv.Served()
	m.kick()
	waitState(t, m, "old", StateDone)
	if got, _ := os.ReadFile(filepath.Join(files, "out/old.bin")); !bytes.Equal(got, data) {
		t.Fatal("continued download differs")
	}
	if n := srv.Served() - served; n > 19<<20 {
		t.Fatalf("continued download fetched %d bytes", n)
	}
}
