package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cestbon0309/crypta/core/crypt"
	"github.com/cestbon0309/crypta/core/engine"
)

func TestStreamBytesCounter(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store")
	files := filepath.Join(dir, "files")
	os.MkdirAll(files, 0o755)
	payload := make([]byte, 8192)
	for i := range payload {
		payload[i] = byte(i)
	}
	os.WriteFile(filepath.Join(files, "clip.bin"), payload, 0o644)

	fs, err := engine.New(&engine.Config{
		Backend:   "local",
		LocalRoot: store,
		Crypt:     crypt.Options{Password: "pw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tm, err := engine.NewManager(filepath.Join(dir, "state"), files)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Transfers: tm, Auth: func(tok string) bool { return tok == "ok" }}
	srv.SetFS(fs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tm.Run(ctx)

	tr, err := tm.Add(engine.Transfer{Kind: engine.KindUpload, Local: "clip.bin", Remote: ""})
	if err != nil {
		t.Fatal(err)
	}
	for {
		list := tm.List()
		if len(list) == 1 && list[0].ID == tr.ID && list[0].State == engine.StateDone {
			break
		}
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	streamBytes := func() int64 {
		req, _ := http.NewRequest("GET", ts.URL+"/api/stream", nil)
		req.Header.Set("Authorization", "Bearer ok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct{ Bytes int64 }
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Bytes
	}

	if got := streamBytes(); got != 0 {
		t.Fatalf("initial stream bytes = %d, want 0", got)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/api/file?path=clip.bin", nil)
	req.Header.Set("Authorization", "Bearer ok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if n != int64(len(payload)) {
		t.Fatalf("read %d bytes, want %d", n, len(payload))
	}

	if got := streamBytes(); got != int64(len(payload)) {
		t.Fatalf("stream bytes = %d, want %d", got, len(payload))
	}
}
