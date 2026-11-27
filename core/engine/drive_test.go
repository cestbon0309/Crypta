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
		} else if s.Done > 16*1024*1024 {
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

	// Download: interrupt, then resume from the partial file.
	dl, _ := m.Add(Transfer{Kind: KindDownload, Remote: "videos/movie.mkv", Local: "out/movie.mkv"})
	for {
		if fi, err := os.Stat(filepath.Join(files, "out/movie.mkv.part")); err == nil && fi.Size() > 1024*1024 {
			break
		}
		if s := waitState(t, m, dl.ID, StateRunning, StateDone, StateQueued); s.State == StateDone {
			break
		}
		time.Sleep(time.Millisecond)
	}
	m.Act(dl.ID, "pause")
	time.Sleep(100 * time.Millisecond)
	m.Act(dl.ID, "resume")
	waitState(t, m, dl.ID, StateDone)
	if got, _ := os.ReadFile(filepath.Join(files, "out/movie.mkv")); !bytes.Equal(got, data) {
		t.Fatal("download differs")
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
