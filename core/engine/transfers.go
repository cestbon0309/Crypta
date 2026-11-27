package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cestbon0309/crypta/core/crypt"
	"github.com/cestbon0309/crypta/core/remote"
)

const (
	KindUpload   = "upload"   // local file -> encrypted folder
	KindDownload = "download" // encrypted folder -> local file
	KindURL      = "url"      // http(s) link -> local disk -> encrypted folder
	KindPikPak   = "pikpak"   // PikPak file or folder -> local disk -> one upload per file

	StateQueued   = "queued"
	StateRunning  = "running"
	StatePaused   = "paused"
	StateDone     = "done"
	StateFailed   = "failed"
	StateCanceled = "canceled"
)

// uploadChunk is how many bytes one request sends to the remote, a multiple
// of remote.ChunkAlign. Each running upload holds one chunk in memory.
var uploadChunk int64 = 128 * 1024 * 1024

// Transfer is one job. It is persisted after every chunk so that a killed
// process continues where it stopped.
type Transfer struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	State  string `json:"state"`
	Phase  string `json:"phase,omitempty"` // "downloading" or "uploading" for url jobs
	Source string `json:"source,omitempty"`
	// Local is relative to the files directory unless absolute.
	Local        string    `json:"local,omitempty"`
	Remote       string    `json:"remote"` // full path inside the encrypted folder
	URL          string    `json:"url,omitempty"`
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	Done         int64     `json:"done"`
	Speed        int64     `json:"speed"` // bytes per second
	Error        string    `json:"error,omitempty"`
	DeleteSource bool      `json:"deleteSource,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	FinishedAt   time.Time `json:"finishedAt,omitzero"`

	// Upload resume state.
	Nonce   []byte    `json:"nonce,omitempty"`
	Session string    `json:"session,omitempty"`
	SrcMod  time.Time `json:"srcMod,omitzero"`
	// ETag or Last-Modified of a partly fetched URL, sent as If-Range.
	Validator string `json:"validator,omitempty"`

	cancel context.CancelFunc
}

type Manager struct {
	mu sync.Mutex
	// naming serializes picking the remote name of a new upload.
	naming    sync.Mutex
	fs        *FS
	stateFile string
	filesDir  string
	workDir   string
	items     map[string]*Transfer
	wake      chan struct{}
	lastSave  time.Time
	Workers   int
	// RetryDelay is the base wait before a failed upload chunk is retried.
	RetryDelay time.Duration
	// AllowPrivateURLs lets URL jobs reach loopback, private and link-local
	// addresses (cloud metadata, services next to the server).
	AllowPrivateURLs bool
	// PikPak enables jobs that fetch from a PikPak account.
	PikPak *PikPak
}

func publicOnly(network, address string, _ syscall.RawConn) error {
	host, _, _ := net.SplitHostPort(address)
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(ip) {
		return fmt.Errorf("%s is a private address; set CRYPTA_ALLOW_PRIVATE_URLS=1 to allow it", host)
	}
	return nil
}

// The check runs on the address actually dialed, so redirects and DNS
// answers pointing inwards are covered too.
var publicClient = &http.Client{Transport: &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: 30 * time.Second, Control: publicOnly}).DialContext,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 60 * time.Second,
	MaxIdleConns:          8,
	IdleConnTimeout:       30 * time.Second,
}}

// NewManager loads the queue from stateDir. filesDir is the base for
// relative local paths (the app's Documents directory on iOS, whose absolute
// path changes between launches).
func NewManager(stateDir, filesDir string) (*Manager, error) {
	m := &Manager{stateFile: filepath.Join(stateDir, "transfers.json"), filesDir: filesDir, workDir: filepath.Join(stateDir, "work"),
		items: map[string]*Transfer{}, wake: make(chan struct{}, 1), Workers: 2, RetryDelay: 2 * time.Second}
	if err := os.MkdirAll(m.workDir, 0o755); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(m.stateFile); err == nil {
		var list []*Transfer
		if json.Unmarshal(b, &list) == nil {
			for _, t := range list {
				if t.State == StateRunning {
					t.State = StateQueued
				}
				t.Speed = 0
				m.items[t.ID] = t
			}
		}
	}
	return m, nil
}

// SetFS attaches (or replaces) the file system and starts queued work.
func (m *Manager) SetFS(fs *FS) {
	m.mu.Lock()
	m.fs = fs
	m.mu.Unlock()
	m.kick()
}

func (m *Manager) Run(ctx context.Context) {
	for {
		m.schedule(ctx)
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		}
	}
}

func (m *Manager) kick() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) schedule(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fs == nil || ctx.Err() != nil {
		return
	}
	running := 0
	var queued []*Transfer
	for _, t := range m.items {
		switch t.State {
		case StateRunning:
			running++
		case StateQueued:
			if t.cancel == nil { // the previous worker has exited
				queued = append(queued, t)
			}
		}
	}
	sort.Slice(queued, func(i, j int) bool { return queued[i].CreatedAt.Before(queued[j].CreatedAt) })
	for _, t := range queued {
		if running >= m.Workers {
			break
		}
		running++
		t.State, t.Error = StateRunning, ""
		tctx, cancel := context.WithCancel(ctx)
		t.cancel = cancel
		go m.run(tctx, t, m.fs)
	}
	m.saveLocked(true)
}

func (m *Manager) run(ctx context.Context, t *Transfer, fs *FS) {
	var err error
	switch t.Kind {
	case KindUpload:
		err = m.upload(ctx, t, fs)
	case KindDownload:
		err = m.download(ctx, t, fs)
	case KindURL:
		if err = m.fetch(ctx, t); err == nil {
			err = m.upload(ctx, t, fs)
		}
	case KindPikPak:
		err = m.pull(ctx, t)
	default:
		err = fmt.Errorf("unknown kind %q", t.Kind)
	}
	m.mu.Lock()
	t.cancel, t.Speed = nil, 0
	if t.State == StateRunning { // not paused or canceled meanwhile
		if err != nil && ctx.Err() != nil {
			t.State = StateQueued // shut down mid-transfer: continue next start
		} else if err != nil {
			t.State, t.Error = StateFailed, err.Error()
		} else {
			t.State, t.Done, t.FinishedAt = StateDone, t.Size, time.Now()
		}
	}
	if t.State == StateDone || t.State == StateCanceled {
		m.cleanup(t)
	}
	m.saveLocked(true)
	m.mu.Unlock()
	m.kick()
}

func (m *Manager) cleanup(t *Transfer) {
	os.RemoveAll(filepath.Join(m.workDir, t.ID))
	if t.DeleteSource && t.Kind == KindUpload && t.State == StateDone {
		os.Remove(m.abs(t.Local))
		m.prune(m.abs(t.Local))
	}
	if t.Kind == KindPikPak && t.State == StateCanceled {
		os.RemoveAll(m.pikpakDir(t.ID))
	}
	if t.Kind == KindDownload && t.State == StateCanceled {
		os.Remove(m.abs(t.Local) + ".part")
	}
}

func (m *Manager) abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(m.filesDir, filepath.FromSlash(p))
}

func (m *Manager) saveLocked(force bool) {
	if !force && time.Since(m.lastSave) < time.Second {
		return
	}
	m.lastSave = time.Now()
	list := make([]*Transfer, 0, len(m.items))
	for _, t := range m.items {
		list = append(list, t)
	}
	b, _ := json.Marshal(list)
	tmp := m.stateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, m.stateFile)
	}
}

// progress tracks speed and persists the position.
type progress struct {
	m    *Manager
	t    *Transfer
	at   time.Time
	base int64
}

func (p *progress) set(done int64, force bool) {
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	now := time.Now()
	if p.at.IsZero() {
		p.at, p.base = now, done
	} else if el := now.Sub(p.at); el >= time.Second {
		p.t.Speed = int64(float64(done-p.base) / el.Seconds())
		p.at, p.base = now, done
	}
	p.t.Done = done
	p.m.saveLocked(force)
}

func (m *Manager) update(t *Transfer, fn func()) {
	m.mu.Lock()
	fn()
	m.saveLocked(true)
	m.mu.Unlock()
}

func (m *Manager) upload(ctx context.Context, t *Transfer, fs *FS) error {
	src, err := os.Open(m.abs(t.Local))
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return errors.New("folders cannot be uploaded as one job")
	}
	size := fi.Size()
	total := crypt.EncryptedSize(size)
	offset := int64(0)
	resumed := false
	if t.Session != "" && len(t.Nonce) == crypt.NonceSize && t.SrcMod.Equal(fi.ModTime()) && t.Size == size {
		off, done, err := fs.R.UploadStatus(ctx, t.Session, total)
		if err == nil {
			if done {
				parent, _ := split(Clean(t.Remote))
				fs.invalidate(parent)
				return nil
			}
			offset, resumed = off, true
		} else if !errors.Is(err, remote.ErrSessionGone) {
			return err
		}
	}
	if !resumed {
		nonce, err := crypt.NewNonce()
		if err != nil {
			return err
		}
		// An upload never overwrites: a name that is taken, by a file or
		// by an upload still under way, becomes "name (2).ext".
		m.naming.Lock()
		parent, name := split(Clean(t.Remote))
		// Taken before listing: an upload that finishes in between is
		// then in one of the two.
		busy := m.uploading(t.ID)
		items, dc, err := fs.list(ctx, parent, true, true)
		if err != nil {
			m.naming.Unlock()
			return err
		}
		ext := path.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		for i := 2; items[name] != nil || busy[path.Join(parent, name)]; i++ {
			name = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		session, err := fs.R.StartUpload(ctx, dc.id, fs.C.EncryptName(name, false), "", total, fi.ModTime())
		if err != nil {
			m.naming.Unlock()
			return err
		}
		m.update(t, func() {
			t.Remote, t.Name = path.Join(parent, name), name
			t.Nonce, t.Session, t.SrcMod, t.Size, t.Done = nonce[:], session, fi.ModTime(), size, 0
		})
		m.naming.Unlock()
	}
	m.update(t, func() { t.Phase = "uploading" })
	var nonce crypt.Nonce
	copy(nonce[:], t.Nonce)
	buf := make([]byte, min(uploadChunk, total))
	pr := &progress{m: m, t: t}
	// Plain bytes covered by the first enc encrypted ones.
	plainAt := func(enc int64) int64 {
		enc -= crypt.HeaderSize
		if enc <= 0 {
			return 0
		}
		blocks, rem := enc/crypt.BlockSize, enc%crypt.BlockSize
		return blocks*crypt.BlockData + max(0, rem-crypt.BlockOverhead)
	}
	retries := 0
	for {
		n, err := fs.C.EncryptAt(src, size, nonce, offset, buf)
		if err != nil && err != io.EOF {
			return err
		}
		at := offset
		done, err := fs.R.UploadChunk(remote.WithSent(ctx, func(sent int64) { pr.set(plainAt(at+sent), false) }),
			t.Session, offset, buf[:n], total)
		if err != nil {
			if errors.Is(err, remote.ErrSessionGone) {
				m.update(t, func() { t.Session = "" })
				return err
			}
			// The chunk may have been stored partly; ask where to go on.
			if retries++; retries > 5 || ctx.Err() != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(retries) * m.RetryDelay):
			}
			off, fin, serr := fs.R.UploadStatus(ctx, t.Session, total)
			if serr != nil {
				return err
			}
			if fin {
				break
			}
			offset = off
			continue
		}
		retries = 0
		offset += int64(n)
		pr.set(plainAt(offset), true)
		if done {
			break
		}
		if offset >= total {
			return errors.New("remote did not confirm the upload")
		}
	}
	parent, _ := split(Clean(t.Remote))
	fs.invalidate(parent)
	return nil
}

func (m *Manager) download(ctx context.Context, t *Transfer, fs *FS) error {
	it, err := fs.Stat(ctx, t.Remote)
	if err != nil {
		return err
	}
	if it.IsDir {
		return errors.New("folders cannot be downloaded as one job")
	}
	dst := m.abs(t.Local)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst+".part", os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	fi, _ := out.Stat()
	// Resume at the last complete block; restart if the remote file changed.
	pos := fi.Size() / crypt.BlockData * crypt.BlockData
	if t.Size != it.Size || !t.SrcMod.Equal(it.ModTime) || pos > it.Size {
		pos = 0
	}
	if err := out.Truncate(pos); err != nil {
		return err
	}
	if _, err := out.Seek(pos, io.SeekStart); err != nil {
		return err
	}
	m.update(t, func() { t.Size, t.SrcMod, t.Done = it.Size, it.ModTime, pos })
	rc, err := fs.Open(ctx, it, pos, -1)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := m.copy(rc, out, t, pos); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	os.Chtimes(dst+".part", it.ModTime, it.ModTime)
	return os.Rename(dst+".part", dst)
}

func (m *Manager) copy(in io.Reader, out io.Writer, t *Transfer, pos int64) error {
	pr := &progress{m: m, t: t}
	buf := make([]byte, 256*1024)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			pos += int64(n)
			pr.set(pos, false)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// fetch downloads t.URL into the work directory, resuming with a Range
// request when the server allows it, then turns t into an upload.
func (m *Manager) fetch(ctx context.Context, t *Transfer) error {
	if t.Phase == "uploading" && t.Local != "" {
		if _, err := os.Stat(m.abs(t.Local)); err == nil {
			return nil
		}
	}
	dir := filepath.Join(m.workDir, t.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	part := filepath.Join(dir, "download.part")
	out, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	fi, _ := out.Stat()
	pos := fi.Size()
	req, err := http.NewRequestWithContext(ctx, "GET", t.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Crypta/1.0")
	// Without a validator the link may serve different content now.
	if pos > 0 && t.Validator != "" {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", pos))
		req.Header.Set("If-Range", t.Validator)
	}
	client := publicClient
	if m.AllowPrivateURLs {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 206 && pos > 0:
	case resp.StatusCode == 200:
		pos = 0
	case resp.StatusCode == 416 && pos > 0:
		out.Truncate(0)
		return errors.New("download could not be resumed, retry to start over")
	default:
		return fmt.Errorf("download failed: HTTP %s", resp.Status)
	}
	if err := out.Truncate(pos); err != nil {
		return err
	}
	if _, err := out.Seek(pos, io.SeekStart); err != nil {
		return err
	}
	name := t.Name
	if name == "" {
		if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
			name = path.Base(strings.ReplaceAll(params["filename"], "\\", "/"))
		}
	}
	if name == "" {
		if u, err := url.Parse(t.URL); err == nil {
			name = path.Base(u.Path)
		}
	}
	if name == "" || name == "." || name == "/" {
		name = "download-" + t.ID
	}
	validator := resp.Header.Get("ETag")
	if validator == "" || strings.HasPrefix(validator, "W/") {
		validator = resp.Header.Get("Last-Modified")
	}
	m.update(t, func() {
		t.Phase, t.Name, t.Validator = "downloading", name, validator
		if resp.ContentLength >= 0 {
			t.Size = pos + resp.ContentLength
		}
	})
	if err := m.copy(resp.Body, out, t, pos); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	final := filepath.Join(dir, "file")
	if err := os.Rename(part, final); err != nil {
		return err
	}
	m.update(t, func() {
		t.Local, t.Remote, t.Phase, t.Done, t.DeleteSource = final, path.Join(t.Remote, name), "uploading", 0, false
	})
	return nil
}

// ---- public queue operations ----

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Add queues a transfer. For uploads Remote is the destination folder and
// the file keeps its name (or Name, if set); for downloads Remote is the
// file and Local the destination; for URL jobs Remote is the folder. A URL
// of the form pikpak:/path fetches that file or folder from PikPak.
func (m *Manager) Add(t Transfer) (*Transfer, error) {
	t.ID, t.State, t.CreatedAt, t.Done = newID(), StateQueued, time.Now(), 0
	if src, ok := strings.CutPrefix(t.URL, "pikpak:"); ok && t.Kind == KindURL {
		if m.PikPak == nil {
			return nil, errors.New("PikPak is not set up on this server")
		}
		src = path.Clean("/" + strings.TrimSpace(src))
		if src == "/" {
			return nil, errors.New("a PikPak path is required, as in pikpak:/folder/file")
		}
		t.Kind, t.URL, t.Source, t.Local = KindPikPak, "", src, ""
		t.Remote = Clean(t.Remote)
		if t.Name == "" {
			t.Name = path.Base(src)
		}
	}
	switch t.Kind {
	case KindUpload:
		if t.Local == "" {
			return nil, errors.New("local path required")
		}
		fi, err := os.Stat(m.abs(t.Local))
		if err != nil {
			return nil, err
		}
		if fi.IsDir() {
			return nil, errors.New("folders cannot be uploaded as one job")
		}
		if t.Name == "" {
			t.Name = filepath.Base(t.Local)
		}
		t.Size, t.Source = fi.Size(), t.Local
		t.Remote = path.Join(Clean(t.Remote), t.Name)
	case KindDownload:
		if t.Remote == "" {
			return nil, errors.New("remote path required")
		}
		t.Remote = Clean(t.Remote)
		t.Name, t.Source = path.Base(t.Remote), t.Remote
		if t.Local == "" {
			t.Local = path.Join("Downloads", t.Name)
		}
		t.Local = m.freeLocal(t.Local)
	case KindPikPak:
		if t.Source == "" {
			return nil, errors.New("a PikPak path is required")
		}
	case KindURL:
		u, err := url.Parse(t.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, errors.New("an http or https URL is required")
		}
		t.Remote, t.Source, t.Local = Clean(t.Remote), t.URL, ""
	default:
		return nil, fmt.Errorf("unknown kind %q", t.Kind)
	}
	m.mu.Lock()
	m.items[t.ID] = &t
	m.saveLocked(true)
	cp := t
	m.mu.Unlock()
	m.kick()
	return &cp, nil
}

// uploading returns the remote paths that unfinished uploads other than
// id have already started writing to.
func (m *Manager) uploading(id string) map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	busy := map[string]bool{}
	for _, t := range m.items {
		if t.ID != id && t.Kind != KindDownload && t.Session != "" &&
			t.State != StateDone && t.State != StateCanceled {
			busy[t.Remote] = true
		}
	}
	return busy
}

// Move and Mkdir are the FS ones, refused for a name that an unfinished
// upload is writing to: the upload would otherwise land on the same name.
func (m *Manager) Move(ctx context.Context, fs *FS, from, to string) error {
	m.naming.Lock()
	defer m.naming.Unlock()
	if err := m.held(to); err != nil {
		return err
	}
	return fs.Move(ctx, from, to)
}

func (m *Manager) Mkdir(ctx context.Context, fs *FS, p string) error {
	m.naming.Lock()
	defer m.naming.Unlock()
	if err := m.held(p); err != nil {
		return err
	}
	return fs.Mkdir(ctx, p)
}

// held reports an unfinished upload to p or to a folder above it, which
// creating p would create too.
func (m *Manager) held(p string) error {
	busy := m.uploading("")
	for p = Clean(p); p != ""; p, _ = split(p) {
		if busy[p] {
			return fmt.Errorf("an upload to %s is under way", p)
		}
	}
	return nil
}

// freeLocal returns p, or "name (2).ext" and so on when p is taken by a
// file or by another download.
func (m *Manager) freeLocal(p string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	taken := func(p string) bool {
		if _, err := os.Lstat(m.abs(p)); err == nil {
			return true
		}
		for _, t := range m.items {
			if t.Kind == KindDownload && t.Local == p && t.State != StateDone && t.State != StateCanceled {
				return true
			}
		}
		return false
	}
	ext := path.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	for i := 2; taken(p); i++ {
		p = fmt.Sprintf("%s (%d)%s", stem, i, ext)
	}
	return p
}

// Reset cancels and forgets every transfer; used when the storage they
// belong to is replaced.
func (m *Manager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, t := range m.items {
		if t.State != StateDone {
			t.State = StateCanceled
			if t.cancel != nil {
				t.cancel()
			} else {
				m.cleanup(t)
			}
		}
		delete(m.items, id)
	}
	m.saveLocked(true)
}

func (m *Manager) List() []Transfer {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Transfer, 0, len(m.items))
	for _, t := range m.items {
		cp := *t
		cp.Session, cp.Nonce = "", nil // resume secrets stay on disk
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Act applies "pause", "resume", "cancel" or "remove" to a transfer.
func (m *Manager) Act(id, action string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.items[id]
	if t == nil {
		return remote.ErrNotFound
	}
	active := t.State == StateQueued || t.State == StateRunning
	switch action {
	case "pause":
		if active {
			t.State = StatePaused
		}
	case "resume":
		if t.State == StatePaused || t.State == StateFailed {
			t.State, t.Error = StateQueued, ""
		}
	case "cancel", "remove":
		if t.State != StateDone {
			running := t.cancel != nil
			t.State = StateCanceled
			if !running {
				m.cleanup(t)
			}
		}
		if action == "remove" {
			delete(m.items, id)
		}
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if t.cancel != nil && t.State != StateRunning {
		t.cancel()
	}
	m.saveLocked(true)
	go m.kick()
	return nil
}

// ClearFinished drops done, failed and canceled entries from the list.
func (m *Manager) ClearFinished() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, t := range m.items {
		if t.State == StateDone || t.State == StateCanceled {
			delete(m.items, id)
		}
	}
	m.saveLocked(true)
}
