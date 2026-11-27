// Package mobile is the entry point of the Go core inside the iOS app
// (built with gomobile bind). It runs the same HTTP API as the server on a
// loopback port; the SwiftUI app and the video player talk to that.
package mobile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cestbon0309/crypta/core/api"
	"github.com/cestbon0309/crypta/core/auth"
	"github.com/cestbon0309/crypta/core/crypt"
	"github.com/cestbon0309/crypta/core/engine"
)

type saved struct {
	Config      *engine.Config `json:"config"`
	Demo        bool           `json:"demo,omitempty"`
	Server      string         `json:"server,omitempty"`
	ServerToken string         `json:"serverToken,omitempty"`
}

type app struct {
	mu       sync.Mutex
	stateDir string
	saved    saved
	srv      *api.Server
}

var (
	startMu sync.Mutex
	started string
)

// Start launches the core and returns {"url": "http://127.0.0.1:port",
// "token": "..."} as JSON. Calling it again returns the same answer.
// stateDir holds settings and the transfer queue; filesDir is the folder
// that relative local paths refer to (the app's Documents directory).
func Start(stateDir, filesDir string) (string, error) {
	startMu.Lock()
	defer startMu.Unlock()
	if started != "" {
		return started, nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	tm, err := engine.NewManager(stateDir, filesDir)
	if err != nil {
		return "", err
	}
	token := auth.Random(24)
	a := &app{stateDir: stateDir}
	a.srv = &api.Server{Transfers: tm, Auth: func(t string) bool { return api.SameToken(t, token) }, Extra: a.routes}
	if b, err := os.ReadFile(a.path()); err == nil {
		if json.Unmarshal(b, &a.saved) == nil && a.saved.Config != nil {
			if fs, err := engine.New(a.fixPaths(a.saved.Config)); err == nil {
				a.srv.SetFS(fs)
			}
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	go tm.Run(context.Background())
	go http.Serve(ln, a.srv.Handler())
	out, _ := json.Marshal(map[string]string{"url": "http://" + ln.Addr().String(), "token": token})
	started = string(out)
	return started, nil
}

func (a *app) path() string { return filepath.Join(a.stateDir, "config.json") }

// The app container path changes between installs, so the demo store
// location is always derived from the current state directory.
func (a *app) fixPaths(cfg *engine.Config) *engine.Config {
	if cfg.Backend == "local" {
		c := *cfg
		c.LocalRoot = filepath.Join(a.stateDir, "demo-store")
		return &c
	}
	return cfg
}

func (a *app) apply(s saved) error {
	var fs *engine.FS
	if s.Config != nil {
		var err error
		if fs, err = engine.New(a.fixPaths(s.Config)); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := fs.List(ctx, "", true); err != nil {
			return fmt.Errorf("could not open the encrypted folder: %w", err)
		}
	}
	b, _ := json.Marshal(s)
	if err := os.WriteFile(a.path(), b, 0o600); err != nil {
		return err
	}
	a.mu.Lock()
	a.saved = s
	a.mu.Unlock()
	a.srv.SetFS(fs)
	return nil
}

func (a *app) routes(mux *http.ServeMux, s *api.Server) {
	reply := func(w http.ResponseWriter, err error) {
		if err != nil {
			api.Fail(w, err)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		backend := ""
		if a.saved.Config != nil {
			backend = a.saved.Config.Backend
		}
		api.JSON(w, 200, map[string]any{"configured": a.saved.Config != nil, "demo": a.saved.Demo, "backend": backend,
			"server": a.saved.Server, "serverToken": a.saved.ServerToken})
	}
	mux.HandleFunc("GET /api/setup", s.Authed(func(w http.ResponseWriter, r *http.Request) { reply(w, nil) }))
	// List the crypt remotes in an rclone.conf so the user can pick one.
	mux.HandleFunc("POST /api/setup/remotes", s.Authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ RcloneConf string }
		json.NewDecoder(r.Body).Decode(&in)
		api.JSON(w, 200, map[string]any{"remotes": engine.CryptRemotes(in.RcloneConf)})
	}))
	mux.HandleFunc("POST /api/setup/rclone", s.Authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ RcloneConf, Remote string }
		json.NewDecoder(r.Body).Decode(&in)
		cfg, err := engine.FromRcloneConf(in.RcloneConf, in.Remote)
		if err == nil {
			a.mu.Lock()
			next := saved{Config: cfg, Server: a.saved.Server, ServerToken: a.saved.ServerToken}
			a.mu.Unlock()
			err = a.apply(next)
		}
		reply(w, err)
	}))
	mux.HandleFunc("POST /api/setup/demo", s.Authed(func(w http.ResponseWriter, r *http.Request) {
		err := a.apply(saved{Demo: true, Config: &engine.Config{Backend: "local", Crypt: crypt.Options{Password: "demo"}}})
		if err == nil {
			a.seedDemo()
		}
		reply(w, err)
	}))
	mux.HandleFunc("POST /api/setup/pair", s.Authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Server, Code, Name string }
		json.NewDecoder(r.Body).Decode(&in)
		reply(w, a.pair(r.Context(), in.Server, in.Code, in.Name))
	}))
	mux.HandleFunc("POST /api/setup/unpair", s.Authed(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		next := saved{Config: a.saved.Config, Demo: a.saved.Demo}
		a.mu.Unlock()
		reply(w, a.apply(next))
	}))
	mux.HandleFunc("POST /api/setup/reset", s.Authed(func(w http.ResponseWriter, r *http.Request) {
		reply(w, a.apply(saved{}))
	}))
}

// NormalizeServer turns "crypta.example.com" into "https://crypta.example.com".
func NormalizeServer(s string) (string, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s == "" {
		return "", errors.New("server address required")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("server address is not valid")
	}
	return s, nil
}

func (a *app) pair(ctx context.Context, server, code, name string) error {
	server, err := NormalizeServer(server)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"code": code, "name": name})
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server+"/api/pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", server, err)
	}
	defer resp.Body.Close()
	var out struct {
		Token  string
		Config *engine.Config
		Error  string
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if json.Unmarshal(raw, &out) != nil || (resp.StatusCode == 200 && (out.Token == "" || out.Config == nil)) {
		return fmt.Errorf("%s did not answer like a Crypta server (HTTP %d)", server, resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return errors.New(out.Error)
	}
	if out.Config.Backend == "local" {
		// A demo server has no shared storage the phone could reach; keep
		// whatever the phone already has and only link the server.
		a.mu.Lock()
		next := saved{Config: a.saved.Config, Demo: a.saved.Demo, Server: server, ServerToken: out.Token}
		a.mu.Unlock()
		if next.Config == nil {
			next.Demo, next.Config = true, &engine.Config{Backend: "local", Crypt: crypt.Options{Password: "demo"}}
		}
		return a.apply(next)
	}
	return a.apply(saved{Config: out.Config, Server: server, ServerToken: out.Token})
}

// seedDemo puts a few files into an empty demo folder so there is
// something to look at.
func (a *app) seedDemo() {
	fs := a.srv.FS()
	ctx := context.Background()
	if items, err := fs.List(ctx, "", true); err != nil || len(items) > 0 {
		return
	}
	dir := filepath.Join(a.stateDir, "demo-seed")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "欢迎.txt")
	os.WriteFile(p, []byte("这是 Crypta 的演示模式。\n\n文件以 rclone crypt 格式加密后保存在本机，方便你在连接 Google Drive 之前先试用界面。\n在“设置”里导入 rclone.conf 或与服务器配对后，就会切换到你的 Google Drive。\n"), 0o644)
	fs.Mkdir(ctx, "示例文件夹")
	a.srv.Transfers.Add(engine.Transfer{Kind: engine.KindUpload, Local: p, Remote: "", DeleteSource: true})
}
