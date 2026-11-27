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
