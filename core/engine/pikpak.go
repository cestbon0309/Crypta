package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// PikPak is where the pikpakcli program and its settings are.
type PikPak struct {
	Bin string
	// Home holds pikpakcli/config.yml and the login session pikpakcli keeps.
	Home string
}

// NewPikPak writes the account into home for pikpakcli to read.
func NewPikPak(bin, home, username, password, proxy string) (*PikPak, error) {
	dir := filepath.Join(home, "pikpakcli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// JSON strings are YAML strings.
	var b bytes.Buffer
	for _, kv := range [][2]string{{"proxy", proxy}, {"username", username}, {"password", password}} {
		v, _ := json.Marshal(kv[1])
		fmt.Fprintf(&b, "%s: %s\n", kv[0], v)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), b.Bytes(), 0o600); err != nil {
		return nil, err
	}
	return &PikPak{Bin: bin, Home: home}, nil
}

// pikpakTarget is p as pikpakcli must be given it: pikpakcli treats * ? [
// anywhere in the path as a pattern for the last name.
func pikpakTarget(p string) string {
	if !strings.ContainsAny(p, "*?[") {
		return p
	}
	dir, name := path.Split(p)
	return dir + strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace(name)
}

func (m *Manager) pikpakDir(id string) string {
	return filepath.Join(m.filesDir, "pikpak", id)
}

// pull downloads the PikPak file or folder t.Source to the files directory
// and queues an upload for every file in it. pikpakcli resumes what an
// earlier run left unfinished.
func (m *Manager) pull(ctx context.Context, t *Transfer) error {
	dir := m.pikpakDir(t.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	m.update(t, func() { t.Phase = "downloading" })
	cmd := exec.CommandContext(ctx, m.PikPak.Bin, "--config", filepath.Join(m.PikPak.Home, "pikpakcli", "config.yml"),
		"download", "-o", dir, "--", pikpakTarget(t.Source))
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+m.PikPak.Home)
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err
	}
	stop := make(chan struct{})
	go func() {
		pr := &progress{m: m, t: t}
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
				_, size, _ := pulled(dir)
				pr.set(size, false)
			}
		}
	}()
	err := cmd.Wait()
	close(stop)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	files, size, partial := pulled(dir)
	// pikpakcli exits with 0 whatever happened.
	if err != nil || partial || len(files) == 0 || strings.Contains(strings.ToLower(out.String()), "failed") {
		msg := strings.Join(strings.Fields(out.String()), " ")
		if len(msg) > 400 {
			msg = "…" + msg[len(msg)-400:]
		}
		if msg == "" && partial {
			msg = "the download did not finish"
		} else if msg == "" {
			msg = "nothing was downloaded"
		}
		return errors.New("pikpakcli: " + msg)
	}
	for _, f := range files {
		rel, _ := filepath.Rel(dir, f)
		if _, err := m.Add(Transfer{Kind: KindUpload, Local: f, Remote: path.Join(t.Remote, path.Dir(filepath.ToSlash(rel))), DeleteSource: true}); err != nil {
			return err
		}
	}
	m.update(t, func() { t.Size = size })
	return nil
}

// pulled lists the files under dir; partial says that some are unfinished.
func pulled(dir string) (files []string, size int64, partial bool) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if strings.HasSuffix(p, ".pikpakclidownload") {
			partial = true
			return nil
		}
		if fi, err := d.Info(); err == nil {
			size += fi.Size()
			files = append(files, p)
		}
		return nil
	})
	return
}

// prune removes the folders an uploaded PikPak file leaves empty.
func (m *Manager) prune(file string) {
	root := filepath.Join(m.filesDir, "pikpak")
	for dir := filepath.Dir(file); strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			return
		}
	}
}
