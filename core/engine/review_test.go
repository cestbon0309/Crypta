package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cestbon0309/crypta/core/crypt"
	"github.com/cestbon0309/crypta/core/internal/fakedrive"
	"github.com/cestbon0309/crypta/core/remote"
)

func driveRig(t *testing.T) (*Manager, *FS, string) {
	srv := fakedrive.New()
	t.Cleanup(srv.Close)
	fs, err := New(&Config{Backend: "drive", Path: "enc", Crypt: crypt.Options{Password: "pw"},
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
	t.Cleanup(cancel)
	m.SetFS(fs)
	go m.Run(ctx)
	return m, fs, files
}

var fixedTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func read(t *testing.T, fs *FS, p string) []byte {
	ctx := context.Background()
	it, err := fs.Stat(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := fs.Open(ctx, it, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return b
}

// Two uploads into a folder that does not exist yet must share one folder.
func TestConcurrentFolderCreation(t *testing.T) {
	_, fs, _ := driveRig(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dc, err := fs.dir(ctx, "new/sub", true)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = dc.id
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("folder created more than once: %v", ids)
		}
	}
}

// Overwriting a file keeps its Drive id and time but changes the nonce.
func TestPreviewAfterOverwrite(t *testing.T) {
	m, fs, files := driveRig(t)
	p := filepath.Join(files, "a.txt")
	for _, content := range []string{"first version", "other content"} {
		os.WriteFile(p, []byte(content), 0o644)
		os.Chtimes(p, fixedTime, fixedTime)
		tr, err := m.Add(Transfer{Kind: KindUpload, Local: "a.txt", Remote: ""})
		if err != nil {
			t.Fatal(err)
		}
		if s := waitState(t, m, tr.ID, StateDone, StateFailed); s.State != StateDone {
			t.Fatal(s.Error)
		}
		if got := read(t, fs, "a.txt"); string(got) != content {
			t.Fatalf("got %q, want %q", got, content)
		}
	}
}

func TestDownloadNamesDoNotCollide(t *testing.T) {
	m, _, _ := driveRig(t)
	a, _ := m.Add(Transfer{Kind: KindDownload, Remote: "x/report.pdf", Local: "Downloads/report.pdf"})
	b, _ := m.Add(Transfer{Kind: KindDownload, Remote: "y/report.pdf", Local: "Downloads/report.pdf"})
	if a.Local == b.Local {
		t.Fatalf("both downloads write to %s", a.Local)
	}
}

func TestURLJobs(t *testing.T) {
	version := "one"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+version+`"`)
		http.ServeContent(w, r, "f.bin", fixedTime, strings.NewReader(strings.Repeat(version, 1000)))
	}))
	defer srv.Close()
	m, fs, _ := driveRig(t)

	tr, _ := m.Add(Transfer{Kind: KindURL, URL: srv.URL + "/f.bin", Remote: "dl"})
	if s := waitState(t, m, tr.ID, StateDone, StateFailed); s.State != StateFailed || !strings.Contains(s.Error, "private address") {
		t.Fatalf("loopback URL was not refused: %+v", s)
	}

	// A partial file from an older version of the link must not be reused.
	m.AllowPrivateURLs = true
	tr, _ = m.Add(Transfer{Kind: KindURL, URL: srv.URL + "/f.bin", Remote: "dl"})
	m.Act(tr.ID, "pause")
	work := filepath.Join(m.workDir, tr.ID)
	os.MkdirAll(work, 0o755)
	os.WriteFile(filepath.Join(work, "download.part"), []byte("zerozero"), 0o644)
	m.mu.Lock()
	m.items[tr.ID].Validator = `"zero"`
	m.mu.Unlock()
	m.Act(tr.ID, "resume")
	if s := waitState(t, m, tr.ID, StateDone, StateFailed); s.State != StateDone {
		t.Fatal(s.Error)
	}
	if got := read(t, fs, "dl/f.bin"); !bytes.Equal(got, []byte(strings.Repeat("one", 1000))) {
		t.Fatalf("resumed download mixed two versions (%d bytes, starts %q)", len(got), got[:8])
	}
}
