package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cestbon0309/crypta/core/crypt"
	"github.com/cestbon0309/crypta/core/remote"
)

type Item struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`

	id      string
	encSize int64
}

type dirCache struct {
	id    string
	items map[string]*Item // nil until listed
	at    time.Time
}

// FS is the decrypted, path based view of the encrypted folder.
type FS struct {
	R      remote.Remote
	C      *crypt.Cipher
	base   string
	mu     sync.Mutex
	mk     sync.Mutex           // serializes folder creation
	dirs   map[string]*dirCache // by clean path, "" is the root
	nonces map[string]crypt.Nonce
}

const listTTL = 30 * time.Second

func New(cfg *Config) (*FS, error) {
	c, err := crypt.New(cfg.Crypt)
	if err != nil {
		return nil, err
	}
	var r remote.Remote
	switch cfg.Backend {
	case "drive":
		r, err = remote.NewDrive(cfg.Drive)
	case "local":
		r, err = remote.NewLocal(cfg.LocalRoot)
	default:
		err = fmt.Errorf("unknown backend %q", cfg.Backend)
	}
	if err != nil {
		return nil, err
	}
	return &FS{R: r, C: c, base: strings.Trim(cfg.Path, "/"), dirs: map[string]*dirCache{}, nonces: map[string]crypt.Nonce{}}, nil
}

func Clean(p string) string {
	p = path.Clean("/" + p)
	return strings.TrimPrefix(p, "/")
}

func split(p string) (dir, name string) {
	dir, name = path.Split(p)
	return strings.TrimSuffix(dir, "/"), name
}

// find returns the newest entry with that name, the one listings show
// when Drive holds duplicates.
func find(entries []remote.Entry, name string, dir bool) *remote.Entry {
	var best *remote.Entry
	for i := range entries {
		e := &entries[i]
		if e.Name == name && e.IsDir == dir && (best == nil || best.ModTime.Before(e.ModTime)) {
			best = e
		}
	}
	return best
}

// rootID resolves (and optionally creates) the unencrypted base path.
func (f *FS) rootID(ctx context.Context, create bool) (string, error) {
	id := f.R.RootID()
	if f.base == "" {
		return id, nil
	}
	for _, seg := range strings.Split(f.base, "/") {
		entries, err := f.R.List(ctx, id)
		if err != nil {
			return "", err
		}
		if e := find(entries, seg, true); e != nil {
			id = e.ID
		} else if create {
			e, err := f.R.Mkdir(ctx, id, seg)
			if err != nil {
				return "", err
			}
			id = e.ID
		} else {
			return "", fmt.Errorf("folder %q: %w", f.base, remote.ErrNotFound)
		}
	}
	return id, nil
}

// dir returns the cache record of a directory, resolving its id.
func (f *FS) dir(ctx context.Context, p string, create bool) (*dirCache, error) {
	f.mu.Lock()
	dc := f.dirs[p]
	f.mu.Unlock()
	if dc != nil {
		return dc, nil
	}
	var id string
	if p == "" {
		var err error
		if id, err = f.rootID(ctx, true); err != nil {
			return nil, err
		}
	} else {
		parent, name := split(p)
		items, pdc, err := f.list(ctx, parent, create, false)
		if err != nil {
			return nil, err
		}
		if it := items[name]; it != nil && it.IsDir {
			id = it.id
		} else if create {
			// One creator at a time, or two uploads into a new folder
			// would each make their own copy of it.
			f.mk.Lock()
			defer f.mk.Unlock()
			f.mu.Lock()
			dc := f.dirs[p]
			f.mu.Unlock()
			if dc != nil {
				return dc, nil
			}
			// Ask the remote directly: going through list could come
			// back here for the parent while the lock is held.
			enc := f.C.EncryptName(name, true)
			entries, err := f.R.List(ctx, pdc.id)
			if err != nil {
				return nil, err
			}
			if e := find(entries, enc, true); e != nil {
				id = e.ID
			} else {
				e, err := f.R.Mkdir(ctx, pdc.id, enc)
				if err != nil {
					return nil, err
				}
				id = e.ID
			}
			f.invalidate(parent)
		} else {
			return nil, fmt.Errorf("%s: %w", p, remote.ErrNotFound)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirs[p] == nil {
		f.dirs[p] = &dirCache{id: id}
	}
	return f.dirs[p], nil
}

func (f *FS) list(ctx context.Context, p string, create, fresh bool) (map[string]*Item, *dirCache, error) {
	dc, err := f.dir(ctx, p, create)
	if err != nil {
		return nil, nil, err
	}
	f.mu.Lock()
	items, at := dc.items, dc.at
	f.mu.Unlock()
	if items != nil && !fresh && time.Since(at) < listTTL {
		return items, dc, nil
	}
	entries, err := f.R.List(ctx, dc.id)
	if err != nil {
		if errors.Is(err, remote.ErrNotFound) {
			f.invalidate(p)
			f.mu.Lock()
			delete(f.dirs, p)
			f.mu.Unlock()
		}
		return nil, nil, err
	}
	items = map[string]*Item{}
	for _, e := range entries {
		name, err := f.C.DecryptName(e.Name, e.IsDir)
		if err != nil || name == "" {
			continue // not part of the encrypted set
		}
		it := &Item{Name: name, Path: path.Join(p, name), IsDir: e.IsDir, ModTime: e.ModTime, id: e.ID, encSize: e.Size}
		if !e.IsDir {
			if it.Size, err = crypt.DecryptedSize(e.Size); err != nil {
				continue
			}
		}
		// Drive allows duplicate names; keep the newest like rclone does.
		if old := items[name]; old == nil || old.ModTime.Before(it.ModTime) {
			items[name] = it
		}
	}
	f.mu.Lock()
	dc.items, dc.at = items, time.Now()
	// Another client may have replaced files or folders under the same
	// names: drop cached headers, and cached folders whose id changed.
	clear(f.nonces)
	prefix := p + "/"
	if p == "" {
		prefix = ""
	}
	for k := range f.dirs {
		if k == p || !strings.HasPrefix(k, prefix) {
			continue
		}
		child, _, _ := strings.Cut(k[len(prefix):], "/")
		if top := f.dirs[prefix+child]; top == nil || items[child] == nil || !items[child].IsDir || items[child].id != top.id {
			delete(f.dirs, k)
		}
	}
	f.mu.Unlock()
	return items, dc, nil
}

func (f *FS) invalidate(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.nonces)
	if dc := f.dirs[p]; dc != nil {
		dc.items = nil
	}
}

// forget drops a directory and everything below it from the cache.
func (f *FS) forget(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.dirs {
		if k == p || strings.HasPrefix(k, p+"/") {
			delete(f.dirs, k)
		}
	}
}

func (f *FS) List(ctx context.Context, p string, fresh bool) ([]Item, error) {
	items, _, err := f.list(ctx, Clean(p), false, fresh)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(items))
	for _, it := range items {
		out = append(out, *it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (f *FS) Stat(ctx context.Context, p string) (*Item, error) {
	p = Clean(p)
	if p == "" {
		return &Item{IsDir: true}, nil
	}
	parent, name := split(p)
	items, _, err := f.list(ctx, parent, false, false)
	if err != nil {
		return nil, err
	}
	if it := items[name]; it != nil {
		return it, nil
	}
	return nil, fmt.Errorf("%s: %w", p, remote.ErrNotFound)
}

func (f *FS) Mkdir(ctx context.Context, p string) error {
	_, err := f.dir(ctx, Clean(p), true)
	return err
}

func (f *FS) Delete(ctx context.Context, p string) error {
	it, err := f.Stat(ctx, p)
	if err != nil {
		return err
	}
	if it.id == "" {
		return errors.New("cannot delete the root folder")
	}
	if err := f.R.Delete(ctx, it.id); err != nil {
		return err
	}
	parent, _ := split(it.Path)
	f.invalidate(parent)
	f.forget(it.Path)
	return nil
}

// Move renames and/or moves src to dst (both full paths).
func (f *FS) Move(ctx context.Context, src, dst string) error {
	dst = Clean(dst)
	it, err := f.Stat(ctx, src)
	if err != nil {
		return err
	}
	if it.id == "" || dst == "" {
		return errors.New("invalid move")
	}
	if dst == it.Path || strings.HasPrefix(dst, it.Path+"/") {
		return errors.New("cannot move a folder into itself")
	}
	if _, err := f.Stat(ctx, dst); err == nil {
		return fmt.Errorf("%s already exists", dst)
	}
	srcParent, _ := split(it.Path)
	dstParent, name := split(dst)
	sdc, err := f.dir(ctx, srcParent, false)
	if err != nil {
		return err
	}
	ddc, err := f.dir(ctx, dstParent, true)
	if err != nil {
		return err
	}
	if err := f.R.Move(ctx, it.id, sdc.id, ddc.id, f.C.EncryptName(name, it.IsDir)); err != nil {
		return err
	}
	f.invalidate(srcParent)
	f.invalidate(dstParent)
	f.forget(it.Path)
	return nil
}

type multiCloser struct {
	io.Reader
	c io.Closer
}

func (m multiCloser) Close() error { return m.c.Close() }

// Open returns length plain text bytes of it from offset (length < 0: to the
// end). Only the encrypted blocks that cover the range are fetched.
func (f *FS) Open(ctx context.Context, it *Item, offset, length int64) (io.ReadCloser, error) {
	if offset >= it.Size || length == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	if length < 0 || offset+length > it.Size {
		length = it.Size - offset
	}
	blk := offset / crypt.BlockData
	lastBlk := (offset + length - 1) / crypt.BlockData
	encEnd := crypt.BlockOffset(lastBlk + 1)
	if encEnd > it.encSize {
		encEnd = it.encSize
	}
	key := it.id + "@" + it.ModTime.String()
	f.mu.Lock()
	nonce, have := f.nonces[key]
	f.mu.Unlock()
	var body io.ReadCloser
	var err error
	if !have {
		hdrLen := int64(crypt.HeaderSize)
		if blk == 0 {
			hdrLen = encEnd // one request for header and data
		}
		rc, err := f.R.Open(ctx, it.id, 0, hdrLen)
		if err != nil {
			return nil, err
		}
		hdr := make([]byte, crypt.HeaderSize)
		if _, err := io.ReadFull(rc, hdr); err != nil {
			rc.Close()
			return nil, crypt.ErrBadHeader
		}
		if nonce, err = crypt.ParseHeader(hdr); err != nil {
			rc.Close()
			return nil, err
		}
		f.mu.Lock()
		f.nonces[key] = nonce
		f.mu.Unlock()
		if blk == 0 {
			body = rc
		} else {
			rc.Close()
		}
	}
	if body == nil {
		start := crypt.BlockOffset(blk)
		if body, err = f.R.Open(ctx, it.id, start, encEnd-start); err != nil {
			return nil, err
		}
	}
	dec := f.C.NewDecrypter(body, nonce, blk)
	if skip := offset - blk*crypt.BlockData; skip > 0 {
		if _, err := io.CopyN(io.Discard, dec, skip); err != nil {
			body.Close()
			return nil, err
		}
	}
	return multiCloser{io.LimitReader(dec, length), body}, nil
}
