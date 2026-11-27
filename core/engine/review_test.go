package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// Uploads of one name, whether one after the other or at the same time,
// each keep their own file.
func TestUploadNamesDoNotCollide(t *testing.T) {
	m, fs, files := driveRig(t)
	want := map[string]bool{"a.txt": true, "a (2).txt": true, "a (3).txt": true, "a (4).txt": true}
	add := func(i int) string {
		local := fmt.Sprintf("up%d/a.txt", i)
		os.MkdirAll(filepath.Join(files, filepath.Dir(local)), 0o755)
		os.WriteFile(filepath.Join(files, local), []byte(fmt.Sprint("content ", i)), 0o644)
		tr, err := m.Add(Transfer{Kind: KindUpload, Local: local, Remote: ""})
		if err != nil {
			t.Fatal(err)
		}
		return tr.ID
	}
	first := add(0)
	if s := waitState(t, m, first, StateDone, StateFailed); s.State != StateDone {
		t.Fatal(s.Error)
	}
	ids := []string{first, add(1), add(2), add(3)}
	for i, id := range ids {
		s := waitState(t, m, id, StateDone, StateFailed)
		if s.State != StateDone {
			t.Fatal(s.Error)
		}
		if got := read(t, fs, s.Remote); string(got) != fmt.Sprint("content ", i) {
			t.Fatalf("%s holds %q", s.Remote, got)
		}
		if !want[s.Name] {
			t.Fatalf("unexpected or repeated name %q", s.Name)
		}
		delete(want, s.Name)
	}
	items, err := fs.List(context.Background(), "", true)
	if err != nil || len(items) != 4 {
		t.Fatalf("listing: %v %+v", err, items)
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

// Two clients that upload one name at the same moment leave Drive with two
// files of that name; both are listed and can be told apart.
func TestDuplicateNamesAreListed(t *testing.T) {
	_, fs, _ := driveRig(t)
	ctx := context.Background()
	if err := fs.Mkdir(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	dc, err := fs.dir(ctx, "d", false)
	if err != nil {
		t.Fatal(err)
	}
	enc := fs.C.EncryptName("a.jpg", false)
	for i := 0; i < 3; i++ {
		data := make([]byte, crypt.EncryptedSize(int64(5+i)))
		session, err := fs.R.StartUpload(ctx, dc.id, enc, "", int64(len(data)), fixedTime)
		if err != nil {
			t.Fatal(err)
		}
		if done, err := fs.R.UploadChunk(ctx, session, 0, data, int64(len(data))); err != nil || !done {
			t.Fatal(done, err)
		}
	}
	names := func() string {
		items, err := fs.List(ctx, "d", true)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, fmt.Sprint(it.Name, ":", it.Size))
		}
		return strings.Join(out, " ")
	}
	first := names()
	if strings.Count(first, "a.jpg:")+strings.Count(first, "a (2).jpg:")+strings.Count(first, "a (3).jpg:") != 3 {
		t.Fatalf("listing: %s", first)
	}
	if again := names(); again != first {
		t.Fatalf("listing changed: %s then %s", first, again)
	}
	// Renaming a shown duplicate gives it that name on Drive.
	if err := fs.Move(ctx, "d/a (3).jpg", "d/b.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(ctx, "d/a (2).jpg"); err != nil {
		t.Fatal(err)
	}
	if got := names(); strings.Count(got, ":") != 2 || !strings.Contains(got, "a.jpg:") || !strings.Contains(got, "b.jpg:") {
		t.Fatalf("after rename and delete: %s", got)
	}
}

// A name that a paused upload is writing to cannot be taken by a move or a
// new folder in the meantime.
func TestUploadNameIsHeld(t *testing.T) {
	m, fs, files := driveRig(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(files, "other.txt"), []byte("other"), 0o644)
	tr, _ := m.Add(Transfer{Kind: KindUpload, Local: "other.txt", Remote: ""})
	waitState(t, m, tr.ID, StateDone)
	m.mu.Lock()
	m.items["held"] = &Transfer{ID: "held", Kind: KindUpload, State: StatePaused, Session: "s", Remote: "a (2).txt", Name: "a (2).txt"}
	m.mu.Unlock()
	if err := m.Move(ctx, fs, "other.txt", "a (2).txt"); err == nil {
		t.Fatal("moved onto a name an upload holds")
	}
	if err := m.Mkdir(ctx, fs, "a (2).txt"); err == nil {
		t.Fatal("made a folder on a name an upload holds")
	}
	if err := m.Mkdir(ctx, fs, "a (2).txt/sub"); err == nil {
		t.Fatal("made a folder under a name an upload holds")
	}
	if err := m.Move(ctx, fs, "other.txt", "a (2).txt/other.txt"); err == nil {
		t.Fatal("moved under a name an upload holds")
	}
	if err := m.Move(ctx, fs, "other.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := m.Mkdir(ctx, fs, "dir"); err != nil {
		t.Fatal(err)
	}
}
