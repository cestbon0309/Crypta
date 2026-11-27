// Command crypta is the Crypta server: it takes download jobs (links, files
// dropped into the shared folder by plugins such as the Telegram bot),
// encrypts them in rclone crypt format and uploads them to Google Drive.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mdp/qrterminal/v3"

	"github.com/cestbon0309/crypta/core/api"
	"github.com/cestbon0309/crypta/core/auth"
	"github.com/cestbon0309/crypta/core/crypt"
	"github.com/cestbon0309/crypta/core/engine"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	stateDir  = env("CRYPTA_STATE", "/data")
	downloads = env("CRYPTA_DOWNLOADS", "/downloads")
	publicURL = strings.TrimRight(env("CRYPTA_PUBLIC_URL", ""), "/")
)

func loadConfig() (*engine.Config, error) {
	if os.Getenv("CRYPTA_DEMO") == "1" {
		return &engine.Config{Backend: "local", LocalRoot: filepath.Join(stateDir, "demo-store"), Crypt: crypt.Options{Password: "demo"}}, nil
	}
	p := env("CRYPTA_RCLONE_CONF", "/config/rclone.conf")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("cannot read rclone.conf (%w); mount it at %s or set CRYPTA_DEMO=1 to try without Google Drive", err, p)
	}
	return engine.FromRcloneConf(string(b), os.Getenv("CRYPTA_REMOTE"))
}

func adminToken() string {
	if t := os.Getenv("CRYPTA_ADMIN_TOKEN"); t != "" {
		return t
	}
	p := filepath.Join(stateDir, "admin-token")
	if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
		return strings.TrimSpace(string(b))
	}
	t := auth.Random(24)
	if err := os.WriteFile(p, []byte(t+"\n"), 0o600); err != nil {
		log.Fatal(err)
	}
	return t
}

func serve() error {
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fs, err := engine.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if _, err := fs.List(ctx, "", true); err != nil {
		return fmt.Errorf("cannot list the encrypted folder: %w", err)
	}
	tm, err := engine.NewManager(stateDir, downloads)
	if tm != nil {
		tm.AllowPrivateURLs = os.Getenv("CRYPTA_ALLOW_PRIVATE_URLS") == "1"
	}
	if err != nil {
		return err
	}
	store := &auth.Store{Path: filepath.Join(stateDir, "auth.json")}
	admin := adminToken()
	srv := &api.Server{
		Transfers: tm,
		LocalRoot: downloads,
		Dashboard: true,
		Auth:      func(t string) bool { return api.SameToken(t, admin) || store.Valid(t) },
		Extra: func(mux *http.ServeMux, s *api.Server) {
			// The only unauthenticated call: trade a one-time code for a
			// device token plus everything the phone needs to open the
			// encrypted folder itself.
			mux.HandleFunc("POST /api/pair", func(w http.ResponseWriter, r *http.Request) {
				var in struct{ Code, Name string }
				if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
					api.Fail(w, err)
					return
				}
				token, err := store.Redeem(in.Code, in.Name)
				if err != nil {
					api.JSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
					return
				}
				log.Printf("paired device %q", in.Name)
				api.JSON(w, 200, map[string]any{"token": token, "config": cfg})
			})
		},
	}
	srv.SetFS(fs)
	go tm.Run(ctx)
	addr := env("CRYPTA_LISTEN", ":8080")
	log.Printf("Crypta server listening on %s (dashboard at /, admin token: `crypta token`)", addr)
	return api.Serve(ctx, &http.Server{Addr: addr, Handler: srv.Handler()})
}

func pair() error {
	if publicURL == "" {
		return errors.New("set CRYPTA_PUBLIC_URL to the address your phone reaches this server at, e.g. https://crypta.example.com")
	}
	if u, err := url.Parse(publicURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("CRYPTA_PUBLIC_URL %q is not an http(s) URL", publicURL)
	}
	store := &auth.Store{Path: filepath.Join(stateDir, "auth.json")}
	code, err := store.NewCode()
	if err != nil {
		return err
	}
	link := "crypta://pair?server=" + url.QueryEscape(publicURL) + "&code=" + code
	fmt.Println()
	qrterminal.GenerateWithConfig(link, qrterminal.Config{Level: qrterminal.L, Writer: os.Stdout, HalfBlocks: true, BlackChar: qrterminal.BLACK_BLACK, WhiteChar: qrterminal.WHITE_WHITE, BlackWhiteChar: qrterminal.BLACK_WHITE, WhiteBlackChar: qrterminal.WHITE_BLACK, QuietZone: 2})
	fmt.Printf("\nScan the code in Crypta (Settings > Pair with server), or enter:\n\n  Server: %s\n  Code:   %s\n\nThe code works once and expires in %d minutes.\n", publicURL, code, int(auth.CodeTTL.Minutes()))
	if strings.HasPrefix(publicURL, "http://") {
		fmt.Println("\nWarning: this address is not HTTPS. Pairing sends your Drive token and encryption password; only use http:// on a network you trust.")
	}
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: crypta <command>

  serve          run the server (default)
  pair           print a QR code / pairing code for the iOS app
  devices        list paired devices
  revoke <id>    remove a paired device
  token          print the admin token (dashboard and plugins)
`)
	}
	flag.Parse()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		log.Fatal(err)
	}
	store := &auth.Store{Path: filepath.Join(stateDir, "auth.json")}
	var err error
	switch flag.Arg(0) {
	case "", "serve":
		err = serve()
	case "pair":
		err = pair()
	case "devices":
		devs := store.Devices()
		if len(devs) == 0 {
			fmt.Println("No paired devices.")
		}
		for _, d := range devs {
			fmt.Printf("%s  %-24s paired %s\n", d.ID, d.Name, d.Created.Format("2006-01-02 15:04"))
		}
	case "revoke":
		if !store.Revoke(flag.Arg(1)) {
			err = fmt.Errorf("no device with id %q", flag.Arg(1))
		}
	case "token":
		fmt.Println(adminToken())
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "crypta:", err)
		os.Exit(1)
	}
}
