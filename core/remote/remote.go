// Package remote is the storage layer underneath the encryption: Google
// Drive, or a plain directory (used by tests and the demo mode).
package remote

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrSessionGone = errors.New("upload session expired")
)

// Entry is one stored (encrypted) object. Names are the raw stored names.
type Entry struct {
	ID      string
	Name    string
	IsDir   bool
	Size    int64
	ModTime time.Time
	// MD5 of the stored bytes, when the remote knows it without reading them.
	MD5 string
}

type Remote interface {
	RootID() string
	List(ctx context.Context, parentID string) ([]Entry, error)
	Mkdir(ctx context.Context, parentID, name string) (Entry, error)
	// Move renames and/or reparents. newParentID == oldParentID for a rename.
	Move(ctx context.Context, id, oldParentID, newParentID, newName string) error
	Delete(ctx context.Context, id string) error
	// Open reads length bytes from offset; length < 0 reads to the end.
	Open(ctx context.Context, id string, offset, length int64) (io.ReadCloser, error)

	// Resumable uploads. The session string survives process restarts.
	// replaceID, if set, is an existing file that the upload overwrites.
	StartUpload(ctx context.Context, parentID, name, replaceID string, size int64, modTime time.Time) (session string, err error)
	// UploadStatus returns how many bytes the remote has stored so far.
	UploadStatus(ctx context.Context, session string, size int64) (offset int64, done bool, err error)
	// UploadChunk sends data at offset. Every chunk except the last must be
	// a multiple of ChunkAlign bytes.
	UploadChunk(ctx context.Context, session string, offset int64, data []byte, size int64) (done bool, err error)
}

const ChunkAlign = 256 * 1024

type bulkKey struct{}

// WithBulk makes Open use a connection of its own for each stream, so that
// ranges read side by side do not share the throughput of one connection.
func WithBulk(ctx context.Context) context.Context {
	return context.WithValue(ctx, bulkKey{}, true)
}

func bulk(ctx context.Context) bool {
	b, _ := ctx.Value(bulkKey{}).(bool)
	return b
}

type sentKey struct{}

// WithSent makes UploadChunk report, while a chunk is on its way, how many
// of its bytes have been sent so far. A resent chunk starts again from 0.
func WithSent(ctx context.Context, fn func(sent int64)) context.Context {
	return context.WithValue(ctx, sentKey{}, fn)
}

type sentReader struct {
	r  io.Reader
	n  int64
	fn func(int64)
}

func (s *sentReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.n += int64(n)
		s.fn(s.n)
	}
	return n, err
}

// sent wraps a request body so that it reports to the function of WithSent.
func sent(ctx context.Context, r io.Reader) io.Reader {
	fn, _ := ctx.Value(sentKey{}).(func(int64))
	if fn == nil {
		return r
	}
	return &sentReader{r: r, fn: fn}
}
