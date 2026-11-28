package engine

import (
	"context"
	"crypto/md5"
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
	"sync/atomic"
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

// partRequest is the same for the chunks of a chunked file, which are sent
// side by side, each holding one request in memory.
var partRequest int64 = 16 * 1024 * 1024

// DefaultParallel and MaxParallel bound how many chunks of one chunked file
// are uploaded at once.
const (
	DefaultParallel = 8
	MaxParallel     = 16
)

// DefaultSegments and MaxSegments bound how many ranges of one file are
// downloaded at once, each on its own connection.
const (
	DefaultSegments = 4
	MaxSegments     = 16
)

// downloadPiece is the unit a download is resumed by: a piece that was not
// complete when the download stopped is fetched again.
var downloadPiece int64 = 8 * 1024 * 1024

// Part is the upload state of one chunk of a chunked file.
type Part struct {
	Nonce   []byte `json:"nonce,omitempty"`
	Session string `json:"session,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

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
	// Parts is set once the name of a chunked upload has been taken.
	Parts []*Part `json:"parts,omitempty"`
	// Chunk is their size and DirID the folder they go to, which may have
	// been renamed or moved since.
	Chunk int64  `json:"chunk,omitempty"`
	DirID string `json:"dirID,omitempty"`
	// Download resume state: bit i of Have is set once piece i, Piece bytes
	// long, is in the .part file.
	Have  []byte `json:"have,omitempty"`
	Piece int64  `json:"piece,omitempty"`
	// ETag or Last-Modified of a partly fetched URL, sent as If-Range.
	Validator string `json:"validator,omitempty"`
	// External is set while a plugin is still fetching the file of an
	// upload: the plugin reports the progress and no worker runs the job.
	External bool `json:"external,omitempty"`

	seen     time.Time // last progress report of an external job
	seenDone int64

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
	parallel  int
	segments  int
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
		items: map[string]*Transfer{}, wake: make(chan struct{}, 1), Workers: 2, parallel: DefaultParallel, segments: DefaultSegments, RetryDelay: 2 * time.Second}
	if err := os.MkdirAll(m.workDir, 0o755); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(m.stateFile); err == nil {
		var list []*Transfer
		if json.Unmarshal(b, &list) == nil {
			for _, t := range list {
				if t.State == StateRunning && !t.External {
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
			if !t.External {
				running++
			}
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
	if len(t.Parts) > 0 && t.State == StateCanceled && m.fs != nil {
		// The chunks sent so far are not a file and never will be.
		go func(fs *FS, dirID, name string) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			fs.dropLoose(ctx, dirID, name)
		}(m.fs, t.DirID, path.Base(t.Remote))
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

// Parallel is how many chunks of one chunked file are uploaded at once.
func (m *Manager) Parallel() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.parallel
}

// SetParallel changes it for the uploads that start from now on.
func (m *Manager) SetParallel(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.parallel = min(max(n, 1), MaxParallel)
}

// Segments is how many ranges of one file are downloaded at once.
func (m *Manager) Segments() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.segments
}

// SetSegments changes it for the downloads that start from now on.
func (m *Manager) SetSegments(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.segments = min(max(n, 1), MaxSegments)
}

// claim picks the remote name of a new upload and calls start with it while
// the name cannot be taken by anything else. An upload never overwrites: a
// name that is taken, by a file or by an upload still under way, becomes
// "name (2).ext".
func (m *Manager) claim(ctx context.Context, t *Transfer, fs *FS, start func(dc *dirCache, parent, name string) error) error {
	m.naming.Lock()
	defer m.naming.Unlock()
	parent, name := split(Clean(t.Remote))
	// Taken before listing: an upload that finishes in between is then in
	// one of the two.
	busy := m.uploading(t.ID)
	items, dc, err := fs.list(ctx, parent, true, true)
	if err != nil {
		return err
	}
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; items[name] != nil || busy[path.Join(parent, name)]; i++ {
		name = fmt.Sprintf("%s (%d)%s", stem, i, ext)
	}
	return start(dc, parent, name)
}

// plainAt is how many plain bytes the first enc encrypted bytes cover.
func plainAt(enc int64) int64 {
	enc -= crypt.HeaderSize
	if enc <= 0 {
		return 0
	}
	blocks, rem := enc/crypt.BlockSize, enc%crypt.BlockSize
	return blocks*crypt.BlockData + max(0, rem-crypt.BlockOverhead)
}

// send encrypts src (size plain bytes) and sends it through an upload
// session, starting at the encrypted offset, request bytes at a time. report
// gets the plain bytes sent so far; stored says they are confirmed.
func (m *Manager) send(ctx context.Context, fs *FS, src io.ReaderAt, size int64, nonce crypt.Nonce, session string, offset, request int64, report func(plain int64, stored bool)) error {
	total := crypt.EncryptedSize(size)
	buf := make([]byte, min(request, total))
	retries := 0
	for {
		n, err := fs.C.EncryptAt(src, size, nonce, offset, buf)
		if err != nil && err != io.EOF {
			return err
		}
		at := offset
		done, err := fs.R.UploadChunk(remote.WithSent(ctx, func(sent int64) { report(plainAt(at+sent), false) }),
			session, offset, buf[:n], total)
		if err != nil {
			if errors.Is(err, remote.ErrSessionGone) {
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
			off, fin, serr := fs.R.UploadStatus(ctx, session, total)
			if serr != nil {
				return err
			}
			if fin {
				return nil
			}
			offset = off
			continue
		}
		retries = 0
		offset += int64(n)
		report(plainAt(offset), true)
		if done {
			return nil
		}
		if offset >= total {
			return errors.New("remote did not confirm the upload")
		}
	}
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
	same := t.SrcMod.Equal(fi.ModTime()) && t.Size == size
	// A file above the chunk size goes up as chunks, like rclone's chunker
	// stores it. One whose name was taken before keeps the way it started.
	if len(t.Parts) > 0 && same || (t.Session == "" || !same) && size > fs.ChunkSize {
		return m.uploadParts(ctx, t, fs, src, fi)
	}
	total := crypt.EncryptedSize(size)
	offset := int64(0)
	resumed := false
	if t.Session != "" && len(t.Nonce) == crypt.NonceSize && same {
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
		if len(t.Parts) > 0 {
			// The source changed and is now small: its old chunks go.
			if err := fs.dropLoose(ctx, t.DirID, path.Base(t.Remote)); err != nil {
				return err
			}
		}
		nonce, err := crypt.NewNonce()
		if err != nil {
			return err
		}
		err = m.claim(ctx, t, fs, func(dc *dirCache, parent, name string) error {
			session, err := fs.R.StartUpload(ctx, dc.id, fs.C.EncryptName(name, false), "", total, fi.ModTime())
			if err != nil {
				return err
			}
			m.update(t, func() {
				t.Remote, t.Name, t.Parts = path.Join(parent, name), name, nil
				t.Nonce, t.Session, t.SrcMod, t.Size, t.Done = nonce[:], session, fi.ModTime(), size, 0
			})
			return nil
		})
		if err != nil {
			return err
		}
	}
	m.update(t, func() { t.Phase = "uploading" })
	var nonce crypt.Nonce
	copy(nonce[:], t.Nonce)
	pr := &progress{m: m, t: t}
	err = m.send(ctx, fs, src, size, nonce, t.Session, offset, uploadChunk, pr.set)
	if errors.Is(err, remote.ErrSessionGone) {
		m.update(t, func() { t.Session = "" })
	}
	if err != nil {
		return err
	}
	parent, _ := split(Clean(t.Remote))
	fs.invalidate(parent)
	return nil
}

// uploadParts stores a file as rclone chunker does: chunks of fs.ChunkSize,
// m.Parallel of them on their way at a time, then the metadata object that
// makes them one file.
func (m *Manager) uploadParts(ctx context.Context, t *Transfer, fs *FS, src *os.File, fi os.FileInfo) error {
	size, mod, chunk := fi.Size(), fi.ModTime(), fs.ChunkSize
	count := int((size + chunk - 1) / chunk)
	if len(t.Parts) > 0 && t.Chunk > 0 && t.DirID != "" && t.SrcMod.Equal(mod) && t.Size == size {
		// Under way: the chunks keep the size and the folder they started with.
		chunk = t.Chunk
		count = len(t.Parts)
	} else {
		err := m.claim(ctx, t, fs, func(dc *dirCache, parent, name string) error {
			// Chunks under this name are leftovers of an upload given up.
			if err := fs.dropChunks(ctx, dc.id, name); err != nil {
				return err
			}
			parts := make([]*Part, count)
			for i := range parts {
				parts[i] = &Part{}
			}
			m.update(t, func() {
				t.Remote, t.Name, t.Parts, t.Chunk, t.DirID = path.Join(parent, name), name, parts, chunk, dc.id
				t.Nonce, t.Session, t.SrcMod, t.Size, t.Done = nil, "", mod, size, 0
			})
			return nil
		})
		if err != nil {
			return err
		}
	}
	parent, name := split(Clean(t.Remote))
	dirID := t.DirID
	m.update(t, func() { t.Phase = "uploading" })

	// rclone records the MD5 of the whole file; it is read alongside.
	sum := make(chan string, 1)
	go func() {
		h := md5.New()
		if _, err := io.Copy(h, io.NewSectionReader(src, 0, size)); err != nil {
			sum <- ""
			return
		}
		sum <- hex.EncodeToString(h.Sum(nil))
	}()

	partSize := func(i int) int64 { return min(chunk, size-int64(i)*chunk) }
	pr := &progress{m: m, t: t}
	var mu sync.Mutex
	sent := make([]int64, count)
	m.mu.Lock()
	for i, p := range t.Parts {
		if p.Done {
			sent[i] = partSize(i)
		}
	}
	m.mu.Unlock()
	report := func(i int, plain int64, stored bool) {
		mu.Lock()
		sent[i] = plain
		var all int64
		for _, n := range sent {
			all += n
		}
		mu.Unlock()
		pr.set(all, stored)
	}

	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var first error
	slots := make(chan struct{}, m.Parallel())
	for i := range t.Parts {
		if sent[i] > 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-pctx.Done():
				return
			}
			defer func() { <-slots }()
			err := m.sendPart(pctx, t, fs, dirID, name, i, io.NewSectionReader(src, int64(i)*chunk, partSize(i)), mod,
				func(plain int64, stored bool) { report(i, plain, stored) })
			if err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
				cancel()
			}
		}()
	}
	wg.Wait()
	if first != nil {
		return first
	}
	hash := <-sum
	if hash == "" {
		return errors.New("could not read the file for its checksum")
	}
	// Chunks deleted behind the upload's back would make a short file.
	if n, err := fs.countChunks(ctx, dirID, name); err != nil {
		return err
	} else if n != count {
		m.update(t, func() { t.Parts = nil })
		return fmt.Errorf("%d of %d chunks are on the remote; the upload starts over", n, count)
	}
	meta := fmt.Sprintf(`{"ver":1,"size":%d,"nchunks":%d,"md5":"%s"}`, size, count, hash)
	nonce, err := crypt.NewNonce()
	if err != nil {
		return err
	}
	session, err := fs.R.StartUpload(ctx, dirID, fs.C.EncryptName(name, false), "", crypt.EncryptedSize(int64(len(meta))), mod)
	if err != nil {
		return err
	}
	if err := m.send(ctx, fs, strings.NewReader(meta), int64(len(meta)), nonce, session, 0, partRequest, func(int64, bool) {}); err != nil {
		return err
	}
	fs.invalidate(parent)
	fs.invalidateID(dirID)
	return nil
}

// sendPart uploads chunk i, going on where an earlier run stopped.
func (m *Manager) sendPart(ctx context.Context, t *Transfer, fs *FS, dirID, name string, i int, src *io.SectionReader, mod time.Time, report func(int64, bool)) error {
	m.mu.Lock()
	p := *t.Parts[i]
	m.mu.Unlock()
	size := src.Size()
	total := crypt.EncryptedSize(size)
	offset := int64(0)
	if p.Session != "" && len(p.Nonce) == crypt.NonceSize {
		off, done, err := fs.R.UploadStatus(ctx, p.Session, total)
		switch {
		case err == nil && done:
			m.update(t, func() { t.Parts[i].Done, t.Parts[i].Session = true, "" })
			report(size, true)
			return nil
		case err == nil:
			offset = off
		case errors.Is(err, remote.ErrSessionGone):
			p.Session = ""
		default:
			return err
		}
	} else {
		p.Session = ""
	}
	if p.Session == "" {
		nonce, err := crypt.NewNonce()
		if err != nil {
			return err
		}
		if p.Session, err = fs.R.StartUpload(ctx, dirID, fs.C.EncryptName(chunkFile(name, i), false), "", total, mod); err != nil {
			return err
		}
		p.Nonce = nonce[:]
		m.update(t, func() { t.Parts[i].Nonce, t.Parts[i].Session = p.Nonce, p.Session })
	}
	var nonce crypt.Nonce
	copy(nonce[:], p.Nonce)
	err := m.send(ctx, fs, src, size, nonce, p.Session, offset, partRequest, report)
	if errors.Is(err, remote.ErrSessionGone) {
		m.update(t, func() { t.Parts[i].Session = "" })
	}
	if err != nil {
		return err
	}
	m.update(t, func() { t.Parts[i].Done, t.Parts[i].Session = true, "" })
	return nil
}

func hasBit(b []byte, i int) bool { return b[i/8]&(1<<(i%8)) != 0 }

// download fetches the file in pieces, m.Segments ranges at a time: the
// file is shared out between that many streams, and one that is through
// takes over half of what the slowest still has to read.
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
	out, err := os.OpenFile(dst+".part", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	fi, err := out.Stat()
	if err != nil {
		return err
	}
	n := int((it.Size + downloadPiece - 1) / downloadPiece)
	pieceLen := func(i int) int64 { return min(downloadPiece, it.Size-int64(i)*downloadPiece) }
	var total atomic.Int64
	m.mu.Lock()
	same := t.Size == it.Size && t.SrcMod.Equal(it.ModTime)
	if !same || t.Piece != downloadPiece || len(t.Have) != (n+7)/8 || fi.Size() != it.Size {
		have := make([]byte, (n+7)/8)
		if same && t.Piece == 0 && fi.Size() < it.Size {
			// A part file written front to back by an earlier version. One
			// already at full size may have been extended below without the
			// bitmap reaching the disk, so it is not trusted.
			for i := 0; i < int(fi.Size()/downloadPiece); i++ {
				have[i/8] |= 1 << (i % 8)
			}
		}
		t.Have = have
	}
	missing := 0
	for i := 0; i < n; i++ {
		if hasBit(t.Have, i) {
			total.Add(pieceLen(i))
		} else {
			missing++
		}
	}
	t.Size, t.SrcMod, t.Piece, t.Done = it.Size, it.ModTime, downloadPiece, total.Load()
	workers := min(m.segments, missing)
	m.saveLocked(true)
	m.mu.Unlock()
	if err := out.Truncate(it.Size); err != nil {
		return err
	}

	// A stream reads the pieces from cur to stop. Guarded by m.mu, like t.Have.
	type stream struct{ cur, stop int }
	streams := map[*stream]bool{}
	taken := make([]bool, n) // inside a stream
	free := func(i int) bool { return !taken[i] && !hasBit(t.Have, i) }
	// pick gives a new stream its share of the largest stretch nobody is
	// reading, or else the second half of the stream with the most left.
	pick := func() (s *stream, off, end int64) {
		m.mu.Lock()
		defer m.mu.Unlock()
		start, length, all := -1, 0, 0
		for i := 0; i < n; i++ {
			if !free(i) {
				continue
			}
			j := i
			for j < n && free(j) {
				j++
			}
			if all += j - i; j-i > length {
				start, length = i, j-i
			}
			i = j
		}
		s = &stream{}
		if start >= 0 {
			s.cur, s.stop = start, start+min(length, (all+workers-1)/workers)
		} else {
			var most *stream
			for o := range streams {
				if most == nil || o.stop-o.cur > most.stop-most.cur {
					most = o
				}
			}
			if most == nil || most.stop-most.cur < 3 {
				return nil, 0, 0
			}
			s.cur, s.stop = most.stop-(most.stop-most.cur-1)/2, most.stop
			most.stop = s.cur
		}
		for i := s.cur; i < s.stop; i++ {
			taken[i] = true
		}
		streams[s] = true
		return s, int64(s.cur) * downloadPiece, min(int64(s.stop)*downloadPiece, it.Size)
	}
	// finish records the piece s has read and says whether s goes on.
	finish := func(s *stream) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		t.Have[s.cur/8] |= 1 << (s.cur % 8)
		taken[s.cur] = false
		s.cur++
		m.saveLocked(false)
		if s.cur >= s.stop {
			delete(streams, s)
			return false
		}
		return true
	}
	drop := func(s *stream) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := s.cur; i < s.stop; i++ {
			taken[i] = false
		}
		delete(streams, s)
	}
	pr := &progress{m: m, t: t}
	var fails atomic.Int32
	ctx, cancel := context.WithCancel(remote.WithBulk(ctx))
	defer cancel()
	work := func() error {
		buf := make([]byte, 256*1024)
		for {
			s, off, end := pick()
			if s == nil {
				return nil
			}
			var got int64 // of piece s.cur, which only this worker changes
			rc, err := fs.Open(ctx, it, off, end-off)
			for err == nil {
				left := pieceLen(s.cur) - got
				if left == 0 {
					fails.Store(0)
					got = 0
					if !finish(s) {
						break
					}
					continue
				}
				var k int
				k, err = rc.Read(buf[:min(left, int64(len(buf)))])
				if k > 0 {
					if _, werr := out.WriteAt(buf[:k], int64(s.cur)*downloadPiece+got); werr != nil {
						rc.Close()
						return werr
					}
					got += int64(k)
					pr.set(total.Add(int64(k)), false)
					if err == io.EOF {
						err = nil
					}
				} else if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
			}
			if rc != nil {
				rc.Close()
			}
			if err == nil {
				continue
			}
			drop(s)
			total.Add(-got)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if fails.Add(1) > 5 {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.RetryDelay):
			}
		}
	}
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		go func() {
			err := work()
			if err != nil {
				cancel()
			}
			errs <- err
		}()
	}
	for w := 0; w < workers; w++ {
		if e := <-errs; e != nil && (err == nil || errors.Is(err, context.Canceled)) {
			err = e
		}
	}
	if err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	m.update(t, func() { t.Have = nil })
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

// AddExternal lists an upload whose file a plugin is still fetching. The
// plugin reports how far it is with Report and hands the file over with
// Ready, or gives up with Fail.
func (m *Manager) AddExternal(name, remoteDir string, size int64) (*Transfer, error) {
	name = path.Base(filepath.ToSlash(name))
	if name == "" || name == "." || name == "/" {
		return nil, errors.New("name required")
	}
	t := &Transfer{ID: newID(), Kind: KindUpload, State: StateRunning, Phase: "downloading", External: true,
		Name: name, Remote: path.Join(Clean(remoteDir), name), Size: max(size, 0), CreatedAt: time.Now()}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[t.ID] = t
	m.saveLocked(true)
	cp := *t
	return &cp, nil
}

// external returns the job a plugin is fetching; m.mu is held.
func (m *Manager) external(id string) (*Transfer, error) {
	t := m.items[id]
	if t == nil {
		return nil, remote.ErrNotFound
	}
	if !t.External {
		return nil, errors.New("not a job a plugin is fetching")
	}
	return t, nil
}

// Report records the progress of an external job and returns it, so that
// the plugin learns of a cancel.
func (m *Manager) Report(id string, done, size int64) (*Transfer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.external(id)
	if err != nil {
		return nil, err
	}
	if t.State == StateRunning {
		now := time.Now()
		if t.seen.IsZero() {
			t.seen, t.seenDone = now, done
		} else if el := now.Sub(t.seen); el >= time.Second {
			t.Speed = max(int64(float64(done-t.seenDone)/el.Seconds()), 0)
			t.seen, t.seenDone = now, done
		}
		t.Done = max(done, 0)
		if size > 0 {
			t.Size = size
		}
		m.saveLocked(false)
	}
	cp := *t
	return &cp, nil
}

// Ready queues the upload of the file an external job has fetched.
func (m *Manager) Ready(id, local string, deleteSource bool) (*Transfer, error) {
	fi, err := os.Stat(m.abs(local))
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, errors.New("folders cannot be uploaded as one job")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.external(id)
	if err != nil {
		return nil, err
	}
	if t.State != StateRunning {
		return nil, fmt.Errorf("the job is %s", t.State)
	}
	t.External, t.State, t.Phase, t.Speed, t.Done = false, StateQueued, "uploading", 0, 0
	t.Local, t.Source, t.Size, t.DeleteSource = local, local, fi.Size(), deleteSource
	m.saveLocked(true)
	cp := *t
	go m.kick()
	return &cp, nil
}

// Fail ends an external job whose file could not be fetched.
func (m *Manager) Fail(id, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.external(id)
	if err != nil {
		return err
	}
	if t.State == StateRunning {
		t.State, t.Error, t.Speed = StateFailed, msg, 0
		m.saveLocked(true)
	}
	return nil
}

// uploading returns the remote paths that unfinished uploads other than
// id have already started writing to.
func (m *Manager) uploading(id string) map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	busy := map[string]bool{}
	for _, t := range m.items {
		if t.ID != id && t.Kind != KindDownload && (t.Session != "" || len(t.Parts) > 0) &&
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
		cp.Session, cp.Nonce, cp.Parts = "", nil, nil // resume secrets stay on disk
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
		if active && !t.External {
			t.State = StatePaused
		}
	case "resume":
		if (t.State == StatePaused || t.State == StateFailed) && !t.External {
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
