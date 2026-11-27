package remote

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Local stores objects in a directory. IDs are paths relative to the root.
type Local struct{ Root string }

func NewLocal(root string) (*Local, error) {
	return &Local{Root: root}, os.MkdirAll(root, 0o755)
}

func (l *Local) p(id string) string { return filepath.Join(l.Root, filepath.FromSlash(id)) }
func (l *Local) RootID() string     { return "." }

func (l *Local) List(ctx context.Context, parentID string) ([]Entry, error) {
	des, err := os.ReadDir(l.p(parentID))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, de := range des {
		fi, err := de.Info()
		if err != nil || filepath.Ext(de.Name()) == ".crypta-part" {
			continue
		}
		out = append(out, Entry{ID: filepath.ToSlash(filepath.Join(parentID, de.Name())), Name: de.Name(), IsDir: de.IsDir(), Size: fi.Size(), ModTime: fi.ModTime()})
	}
	return out, nil
}

func (l *Local) Mkdir(ctx context.Context, parentID, name string) (Entry, error) {
	id := filepath.ToSlash(filepath.Join(parentID, name))
	return Entry{ID: id, Name: name, IsDir: true, ModTime: time.Now()}, os.MkdirAll(l.p(id), 0o755)
}

func (l *Local) Move(ctx context.Context, id, oldParentID, newParentID, newName string) error {
	return os.Rename(l.p(id), l.p(filepath.Join(newParentID, newName)))
}

func (l *Local) Delete(ctx context.Context, id string) error { return os.RemoveAll(l.p(id)) }

type limitedFile struct {
	io.Reader
	f *os.File
}

func (lf limitedFile) Close() error { return lf.f.Close() }

func (l *Local) Open(ctx context.Context, id string, offset, length int64) (io.ReadCloser, error) {
	f, err := os.Open(l.p(id))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	var r io.Reader = f
	if length >= 0 {
		r = io.LimitReader(f, length)
	}
	return limitedFile{r, f}, nil
}

// The session is the destination id; data accumulates in "<dest>.crypta-part".
func (l *Local) StartUpload(ctx context.Context, parentID, name, replaceID string, size int64, modTime time.Time) (string, error) {
	id := filepath.ToSlash(filepath.Join(parentID, name))
	f, err := os.Create(l.p(id) + ".crypta-part")
	if err != nil {
		return "", err
	}
	return id, f.Close()
}

func (l *Local) UploadStatus(ctx context.Context, session string, size int64) (int64, bool, error) {
	fi, err := os.Stat(l.p(session) + ".crypta-part")
	if err != nil {
		if fi, err2 := os.Stat(l.p(session)); err2 == nil && fi.Size() == size {
			return size, true, nil
		}
		return 0, false, ErrSessionGone
	}
	return fi.Size(), false, nil
}

func (l *Local) UploadChunk(ctx context.Context, session string, offset int64, data []byte, size int64) (bool, error) {
	part := l.p(session) + ".crypta-part"
	f, err := os.OpenFile(part, os.O_WRONLY, 0o644)
	if err != nil {
		return false, ErrSessionGone
	}
	_, err = f.WriteAt(data, offset)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, err
	}
	if offset+int64(len(data)) >= size {
		return true, os.Rename(part, l.p(session))
	}
	return false, nil
}
