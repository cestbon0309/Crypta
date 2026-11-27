// Package api is the HTTP interface of the Crypta core. The iOS app runs it
// on 127.0.0.1 inside the app; the server exposes it to paired devices,
// download plugins and the dashboard.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/cestbon0309/crypta/core/engine"
	"github.com/cestbon0309/crypta/core/remote"
)

//go:embed dashboard.html
var dashboardHTML []byte

type Server struct {
	Transfers *engine.Manager
	// Auth reports whether a bearer token is valid.
	Auth func(token string) bool
	// LocalRoot, if set, confines upload sources to this directory
	// (the server's shared downloads folder).
	LocalRoot string
	// Dashboard serves the progress page at "/".
	Dashboard bool
	// PikPak tells clients that pikpak:/path jobs are accepted.
	PikPak bool
	// Extra routes (pairing on the server, setup on the phone).
	Extra func(mux *http.ServeMux, s *Server)

	mu sync.RWMutex
	fs *engine.FS
}

func (s *Server) SetFS(fs *engine.FS) {
	s.mu.Lock()
	s.fs = fs
	s.mu.Unlock()
	s.Transfers.SetFS(fs)
}

func (s *Server) FS() *engine.FS {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fs
}

func TokenHash(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func SameToken(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func Fail(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, remote.ErrNotFound) {
		status = http.StatusNotFound
	}
	JSON(w, status, map[string]string{"error": err.Error()})
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return h[7:]
	}
	// Media players cannot set headers, so file URLs may carry the token.
	return r.URL.Query().Get("token")
}

// Authed wraps a handler with the token check.
func (s *Server) Authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Auth(bearer(r)) {
			JSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing token"})
			return
		}
		h(w, r)
	}
}

func (s *Server) withFS(h func(w http.ResponseWriter, r *http.Request, fs *engine.FS)) http.HandlerFunc {
	return s.Authed(func(w http.ResponseWriter, r *http.Request) {
		fs := s.FS()
		if fs == nil {
			JSON(w, http.StatusConflict, map[string]string{"error": "not set up yet"})
			return
		}
		h(w, r, fs)
	})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(v)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.Authed(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, 200, map[string]any{"configured": s.FS() != nil, "pikpak": s.PikPak})
	}))
	mux.HandleFunc("GET /api/list", s.withFS(func(w http.ResponseWriter, r *http.Request, fs *engine.FS) {
		items, err := fs.List(r.Context(), r.URL.Query().Get("path"), r.URL.Query().Get("fresh") == "1")
		if err != nil {
			Fail(w, err)
			return
		}
		JSON(w, 200, map[string]any{"items": items})
	}))
	mux.HandleFunc("POST /api/mkdir", s.withFS(func(w http.ResponseWriter, r *http.Request, fs *engine.FS) {
		var in struct{ Path string }
		if err := decode(r, &in); err != nil || engine.Clean(in.Path) == "" {
			Fail(w, errors.New("path required"))
			return
		}
		s.done(w, s.Transfers.Mkdir(r.Context(), fs, in.Path))
	}))
	mux.HandleFunc("POST /api/move", s.withFS(func(w http.ResponseWriter, r *http.Request, fs *engine.FS) {
		var in struct{ From, To string }
		if err := decode(r, &in); err != nil {
			Fail(w, err)
			return
		}
		s.done(w, s.Transfers.Move(r.Context(), fs, in.From, in.To))
	}))
	mux.HandleFunc("POST /api/delete", s.withFS(func(w http.ResponseWriter, r *http.Request, fs *engine.FS) {
		var in struct{ Path string }
		if err := decode(r, &in); err != nil {
			Fail(w, err)
			return
		}
		s.done(w, fs.Delete(r.Context(), in.Path))
	}))
	mux.HandleFunc("GET /api/file", s.withFS(s.file))
	mux.HandleFunc("GET /api/transfers", s.Authed(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, 200, map[string]any{"transfers": s.Transfers.List()})
	}))
	mux.HandleFunc("POST /api/transfers", s.Authed(s.addTransfer))
	mux.HandleFunc("POST /api/transfers/clear", s.Authed(func(w http.ResponseWriter, r *http.Request) {
		s.Transfers.ClearFinished()
		s.done(w, nil)
	}))
	mux.HandleFunc("POST /api/transfers/{id}/progress", s.Authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Done, Size int64 }
		if err := decode(r, &in); err != nil {
			Fail(w, err)
			return
		}
		s.transfer(w)(s.Transfers.Report(r.PathValue("id"), in.Done, in.Size))
	}))
	mux.HandleFunc("POST /api/transfers/{id}/ready", s.Authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Local        string
			DeleteSource bool
		}
		err := decode(r, &in)
		if err == nil {
			in.Local, err = s.local(in.Local)
		}
		if err != nil {
			Fail(w, err)
			return
		}
		s.transfer(w)(s.Transfers.Ready(r.PathValue("id"), in.Local, in.DeleteSource))
	}))
	mux.HandleFunc("POST /api/transfers/{id}/fail", s.Authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Error string }
		if err := decode(r, &in); err != nil {
			Fail(w, err)
			return
		}
		s.done(w, s.Transfers.Fail(r.PathValue("id"), in.Error))
	}))
	mux.HandleFunc("POST /api/transfers/{id}/{action}", s.Authed(func(w http.ResponseWriter, r *http.Request) {
		s.done(w, s.Transfers.Act(r.PathValue("id"), r.PathValue("action")))
	}))
	if s.Dashboard {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Write(dashboardHTML)
		})
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	if s.Extra != nil {
		s.Extra(mux, s)
	}
	return mux
}

func (s *Server) done(w http.ResponseWriter, err error) {
	if err != nil {
		Fail(w, err)
		return
	}
	JSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) transfer(w http.ResponseWriter) func(*engine.Transfer, error) {
	return func(t *engine.Transfer, err error) {
		if err != nil {
			Fail(w, err)
			return
		}
		JSON(w, 200, t)
	}
}

// local resolves the path of a file to upload. On a server, relative paths
// are inside the shared downloads folder and nothing outside of it may be
// uploaded.
func (s *Server) local(p string) (string, error) {
	if s.LocalRoot == "" {
		return p, nil
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.LocalRoot, p)
	}
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	root := filepath.Clean(s.LocalRoot)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return "", fmt.Errorf("local path must be inside %s", s.LocalRoot)
	}
	return p, nil
}

func (s *Server) addTransfer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind, Local, Remote, URL, Name, Source string
		DeleteSource, Pending                  bool
		Size                                   int64
	}
	if err := decode(r, &in); err != nil {
		Fail(w, err)
		return
	}
	if s.LocalRoot != "" {
		if in.Kind == engine.KindDownload {
			Fail(w, errors.New("this server only accepts upload and url jobs"))
			return
		}
	}
	if in.Kind == engine.KindUpload && in.Pending {
		s.transfer(w)(s.Transfers.AddExternal(in.Name, in.Remote, in.Size))
		return
	}
	if in.Kind == engine.KindUpload {
		var err error
		if in.Local, err = s.local(in.Local); err != nil {
			Fail(w, err)
			return
		}
	}
	t, err := s.Transfers.Add(engine.Transfer{Kind: in.Kind, Local: in.Local, Remote: in.Remote, URL: in.URL, Name: in.Name, DeleteSource: in.DeleteSource})
	if err != nil {
		Fail(w, err)
		return
	}
	JSON(w, 200, t)
}

// file streams a decrypted file and honours Range requests, which is what
// lets a video player seek without downloading the whole file.
func (s *Server) file(w http.ResponseWriter, r *http.Request, fs *engine.FS) {
	it, err := fs.Stat(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		Fail(w, err)
		return
	}
	if it.IsDir {
		Fail(w, errors.New("is a folder"))
		return
	}
	ct := mime.TypeByExtension(strings.ToLower(path.Ext(it.Name)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	// Stored files are untrusted: never let one run as a page of this origin.
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Last-Modified", it.ModTime.UTC().Format(http.TimeFormat))
	start, end, status := int64(0), it.Size-1, 200
	if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") && !strings.Contains(rng, ",") {
		a, b, _ := strings.Cut(rng[6:], "-")
		var err1, err2 error
		switch {
		case a == "": // suffix: last N bytes
			var n int64
			n, err1 = strconv.ParseInt(b, 10, 64)
			start = max(it.Size-n, 0)
		case b == "":
			start, err1 = strconv.ParseInt(a, 10, 64)
		default:
			start, err1 = strconv.ParseInt(a, 10, 64)
			end, err2 = strconv.ParseInt(b, 10, 64)
			end = min(end, it.Size-1)
		}
		if err1 != nil || err2 != nil || start < 0 || start > end || start >= it.Size {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", it.Size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		status = 206
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, it.Size))
	}
	length := end - start + 1
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if r.Method == "HEAD" || length <= 0 {
		w.WriteHeader(status)
		return
	}
	rc, err := fs.Open(r.Context(), it, start, length)
	if err != nil {
		w.Header().Del("Content-Length")
		w.Header().Del("Content-Range")
		Fail(w, err)
		return
	}
	defer rc.Close()
	w.WriteHeader(status)
	io.Copy(w, rc)
}

// Serve runs h until ctx ends.
func Serve(ctx context.Context, srv *http.Server) error {
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
