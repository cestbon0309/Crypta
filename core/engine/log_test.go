package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cestbon0309/crypta/core/crypt"
	"github.com/cestbon0309/crypta/core/internal/fakedrive"
	"github.com/cestbon0309/crypta/core/remote"
)

var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var uuidV5Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestUUIDFormatAndThumbID(t *testing.T) {
	id1, id2 := newID(), newID()
	if !uuidV4Re.MatchString(id1) || !uuidV4Re.MatchString(id2) {
		t.Fatalf("newID() not UUID v4: %q, %q", id1, id2)
	}
	if id1 == id2 {
		t.Fatalf("duplicate UUIDs: %q", id1)
	}
	th1 := thumbLogID("drive-file-id-123")
	th2 := thumbLogID("drive-file-id-123")
	th3 := thumbLogID("drive-file-id-456")
	if !uuidV5Re.MatchString(th1) {
		t.Fatalf("thumbLogID not UUID v5: %q", th1)
	}
	if th1 != th2 || th1 == th3 {
		t.Fatalf("thumbLogID determinism failed: %q %q %q", th1, th2, th3)
	}
	if strings.Contains(th1, "drive-file-id") {
		t.Fatalf("thumbLogID leaked item id: %q", th1)
	}
}

func TestRedactErrorZeroFilenameLeak(t *testing.T) {
	fid := newUUID()
	tr := &Transfer{
		ID:     fid,
		Kind:   KindURL,
		Name:   "机密电影 [1080p].mp4",
		Remote: "Movies/私人目录/机密电影 [1080p].mp4",
		Local:  "telegram/" + fid + "/机密电影 [1080p].mp4",
		URL:    "https://cdn.example.com/secret/My%20Movie%202026.mkv?token=xyz",
		Source: "https://cdn.example.com/secret/My%20Movie%202026.mkv?token=xyz",
	}

	cases := []struct {
		name      string
		err       error
		forbidden []string
	}{
		{
			name:      "ffmpeg error with local path",
			err:       fmt.Errorf("ffmpeg: exit status 1: /downloads/telegram/%s/机密电影 [1080p].mp4: Invalid data found", fid),
			forbidden: []string{"机密电影", "[1080p]", ".mp4", "/downloads/"},
		},
		{
			name:      "url.Error with redirected secret filename",
			err:       &url.Error{Op: "Get", URL: "https://other.example.com/redirected/ultra_secret.mkv", Err: errors.New("connection refused")},
			forbidden: []string{"ultra_secret", ".mkv", "other.example.com"},
		},
		{
			name:      "fs.PathError with spaces",
			err:       &fs.PathError{Op: "open", Path: "/downloads/secret folder/private file.zip", Err: os.ErrNotExist},
			forbidden: []string{"secret folder", "private file", ".zip", "/downloads/"},
		},
		{
			name:      "url encoded filename in plain error",
			err:       errors.New("failed to fetch My Movie 2026.mkv from https://cdn.example.com/secret/My%20Movie%202026.mkv?token=xyz"),
			forbidden: []string{"My Movie 2026", "My%20Movie", ".mkv", "token=xyz", "cdn.example.com"},
		},
		{
			name:      "remote folder and filename in fs error",
			err:       fmt.Errorf("Movies/私人目录/机密电影 [1080p].mp4: %w", remote.ErrNotFound),
			forbidden: []string{"Movies", "私人目录", "机密电影", ".mp4"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactTransferError(nil, tr, tc.err)
			for _, bad := range tc.forbidden {
				if strings.Contains(got, bad) {
					t.Errorf("redacted error leaked %q: %q", bad, got)
				}
			}
		})
	}
}

func TestRedactPikPakErrorNeverLeaksChildFilenames(t *testing.T) {
	fid := newUUID()
	tr := &Transfer{ID: fid, Kind: KindPikPak, Source: "/MyFolder"}
	err := errors.New("pikpakcli: Resolve download target failed: /MyFolder/super_secret_episode_01.mkv")
	got := redactTransferError(nil, tr, err)
	if strings.Contains(got, "super_secret") || strings.Contains(got, "MyFolder") || strings.Contains(got, ".mkv") {
		t.Fatalf("pikpak redacted error leaked filename: %q", got)
	}
	if got != "pikpakcli: target not found" {
		t.Fatalf("unexpected pikpak summary: %q", got)
	}
}

func TestTransferLifecycleLogsUseUUIDAndNeverLeakFilenames(t *testing.T) {
	var buf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origOut)

	srv := fakedrive.New()
	defer srv.Close()
	cfg := &Config{
		Backend: "drive",
		Path:    "enc",
		Crypt:   crypt.Options{Password: "pw"},
		Drive:   remote.DriveConfig{ClientID: "id", Token: json.RawMessage(`{"refresh_token":"rt"}`), APIBase: srv.URL, TokenURL: srv.URL + "/token"},
	}
	fsys, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	os.MkdirAll(files, 0o755)
	secretUpload := "top_secret_document_2026.txt"
	os.WriteFile(filepath.Join(files, secretUpload), []byte("hello"), 0o644)

	// HTTP server serving a secret URL that returns 403 to trigger a failed URL download log.
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer httpSrv.Close()

	m, err := NewManager(filepath.Join(dir, "state"), files)
	if err != nil {
		t.Fatal(err)
	}
	m.AllowPrivateURLs = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.SetFS(fsys)
	go m.Run(ctx)

	// 1. Successful upload
	tr1, err := m.Add(Transfer{Kind: KindUpload, Local: secretUpload, Remote: "PrivateFolder", DeleteSource: true})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, tr1.ID, StateDone)

	// 2. Failed URL download
	secretURLName := "hidden_video_leak_check.mp4"
	tr2, err := m.Add(Transfer{Kind: KindURL, URL: httpSrv.URL + "/" + secretURLName, Remote: "PrivateFolder"})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, tr2.ID, StateFailed)

	// 3. External transfer fail with filename in error
	secretExt := "telegram_confidential_archive.zip"
	tr3, err := m.AddExternal(secretExt, "Telegram", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Fail(tr3.ID, "could not write /downloads/telegram/"+tr3.ID+"/"+secretExt+": disk full"); err != nil {
		t.Fatal(err)
	}

	// Give goroutines a moment to flush final log lines.
	time.Sleep(50 * time.Millisecond)

	logs := buf.String()
	for _, id := range []string{tr1.ID, tr2.ID, tr3.ID} {
		if !uuidV4Re.MatchString(id) {
			t.Fatalf("transfer ID not UUID v4: %q", id)
		}
		if !strings.Contains(logs, "file="+id) {
			t.Errorf("expected file=%s in logs:\n%s", id, logs)
		}
	}
	for _, want := range []string{"transfer queued", "transfer started", "transfer done", "transfer failed", "transfer created"} {
		if !strings.Contains(logs, want) {
			t.Errorf("expected lifecycle event %q in logs:\n%s", want, logs)
		}
	}
	for _, forbidden := range []string{secretUpload, secretURLName, secretExt, "PrivateFolder", "/downloads/"} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("logs leaked %q:\n%s", forbidden, logs)
		}
	}
}
