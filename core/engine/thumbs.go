package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cestbon0309/crypta/core/crypt"
	"github.com/cestbon0309/crypta/core/remote"
)

// Thumbnails are small JPEGs in ThumbDir of the encrypted remote, named
// "<file id>.<md5 of the stored file>.<file name>.jpg". The id survives a
// rename or a move of the file; the MD5 changes with its content. Only the
// first two parts are matched, the name is there for people.
const (
	ThumbDir   = ".crypta/thumbs"
	thumbEdge  = 320
	thumbAt    = 0.3 // of a video's length: the first frame is often black
	thumbLimit = 2 * time.Minute
	// A thumbnail asked for and not found is looked for again in a listing
	// that is at most this old: another device may have made it meanwhile.
	thumbFresh = 10 * time.Second
	// How many thumbnails may wait to be made, and how many files that
	// gave none are remembered so that they are not tried again.
	thumbQueue = 5000
)

var (
	plainID   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true, ".tif": true, ".tiff": true, ".heic": true, ".heif": true, ".avif": true}
	videoExts = map[string]bool{".mp4": true, ".m4v": true, ".mov": true, ".mkv": true, ".webm": true, ".avi": true, ".wmv": true, ".flv": true, ".ts": true, ".m2ts": true, ".mpg": true, ".mpeg": true, ".rm": true, ".rmvb": true, ".3gp": true}
)

func thumbKey(it *Item) string {
	id := it.id
	if !plainID.MatchString(id) {
		sum := sha1.Sum([]byte(id))
		id = hex.EncodeToString(sum[:])
	}
	return id + "." + it.md5 + "."
}

// Thumb is the stored thumbnail of a file, or remote.ErrNotFound.
func (f *FS) Thumb(ctx context.Context, it *Item) (*Item, error) {
	if it.IsDir {
		return nil, remote.ErrNotFound
	}
	key := thumbKey(it)
	for fresh := false; ; fresh = true {
		items, dc, err := f.list(ctx, ThumbDir, false, fresh)
		if err != nil {
			return nil, err
		}
		for name, th := range items {
			if !th.IsDir && strings.HasPrefix(name, key) {
				return th, nil
			}
		}
		f.mu.Lock()
		old := time.Since(dc.at) > thumbFresh
		f.mu.Unlock()
		if fresh || !old {
			return nil, fmt.Errorf("thumbnail of %s: %w", it.Path, remote.ErrNotFound)
		}
	}
}

func isMedia(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return imageExts[ext] || videoExts[ext]
}

// frame makes the thumbnail of a picture or video, a local file or a URL,
// with ffmpeg; nil when the name is neither or there is no ffmpeg.
func frame(ctx context.Context, file, name string) ([]byte, error) {
	ext := strings.ToLower(path.Ext(name))
	if !isMedia(name) {
		return nil, nil
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, nil
	}
	args := []string{"-v", "error", "-nostdin"}
	if videoExts[ext] {
		if ffprobe, err := exec.LookPath("ffprobe"); err == nil {
			out, _ := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", file).Output()
			if d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err == nil && d > 0 {
				args = append(args, "-ss", strconv.FormatFloat(d*thumbAt, 'f', 3, 64))
			}
		}
	}
	scale := fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", thumbEdge, thumbEdge)
	args = append(args, "-i", file, "-frames:v", "1", "-vf", scale, "-q:v", "6", "-f", "mjpeg", "pipe:1")
	var out, msg bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.Stdout, cmd.Stderr = &out, &msg
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	said, _, _ := strings.Cut(strings.TrimSpace(msg.String()), "\n")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, said)
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg made no picture: %s", said)
	}
	return out.Bytes(), nil
}

// thumbnail stores the thumbnail of a file that was just uploaded, made
// from the local copy while it is still there.
func (m *Manager) thumbnail(ctx context.Context, t *Transfer, fs *FS) error {
	ctx, cancel := context.WithTimeout(ctx, thumbLimit)
	defer cancel()
	data, err := frame(ctx, m.abs(t.Local), path.Base(t.Remote))
	if data == nil {
		return err
	}
	it, err := fs.Stat(ctx, t.Remote)
	if err != nil {
		return err
	}
	return m.storeThumb(ctx, fs, it, data)
}

func (m *Manager) storeThumb(ctx context.Context, fs *FS, it *Item, data []byte) error {
	dc, err := fs.dir(ctx, ThumbDir, true)
	if err != nil {
		return err
	}
	name := []rune(it.Name)
	if len(name) > 60 {
		name = name[len(name)-60:]
	}
	nonce, err := crypt.NewNonce()
	if err != nil {
		return err
	}
	size := int64(len(data))
	session, err := fs.R.StartUpload(ctx, dc.id, fs.C.EncryptName(thumbKey(it)+string(name)+".jpg", false), "", crypt.EncryptedSize(size), time.Now())
	if err != nil {
		return err
	}
	if err := m.send(ctx, fs, bytes.NewReader(data), size, nonce, session, 0, partRequest, func(int64, bool) {}); err != nil {
		return err
	}
	fs.invalidate(ThumbDir)
	return nil
}

// itemReader reads a stored file from any position, which is what
// http.ServeContent needs to answer range requests.
type itemReader struct {
	ctx context.Context
	f   *FS
	it  *Item
	pos int64
	rc  io.ReadCloser
}

func (r *itemReader) Read(b []byte) (int, error) {
	if r.pos >= r.it.Size {
		return 0, io.EOF
	}
	if r.rc == nil {
		rc, err := r.f.Open(r.ctx, r.it, r.pos, -1)
		if err != nil {
			return 0, err
		}
		r.rc = rc
	}
	n, err := r.rc.Read(b)
	r.pos += int64(n)
	return n, err
}

func (r *itemReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += r.pos
	case io.SeekEnd:
		offset += r.it.Size
	}
	if offset < 0 {
		return 0, errors.New("seek before the start")
	}
	if offset != r.pos {
		r.Close()
	}
	r.pos = offset
	return offset, nil
}

func (r *itemReader) Close() error {
	if r.rc != nil {
		r.rc.Close()
		r.rc = nil
	}
	return nil
}

// remoteFrame is frame for a stored file. ffmpeg reads it from a server on
// the loopback interface that answers range requests, so of a video only
// the index and the frame are fetched, not the whole file.
func remoteFrame(ctx context.Context, fs *FS, it *Item) ([]byte, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		ln.Close()
		return nil, err
	}
	at := "/" + hex.EncodeToString(secret)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != at {
			http.NotFound(w, r)
			return
		}
		rd := &itemReader{ctx: r.Context(), f: fs, it: it}
		defer rd.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", it.ModTime, rd)
	})}
	go srv.Serve(smallBuffers{ln})
	defer srv.Close()
	return frame(ctx, "http://"+ln.Addr().String()+at, it.Name)
}

// RequestThumbs queues the pictures and videos among paths that have no
// thumbnail yet, and says how many it queued. They are made one at a time
// in the background. Without ffmpeg nothing is queued.
func (m *Manager) RequestThumbs(ctx context.Context, fs *FS, paths []string) int {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return 0
	}
	listed := map[string]bool{}
	queued := 0
	for _, p := range paths {
		p = Clean(p)
		if !isMedia(p) || p == ThumbDir || strings.HasPrefix(p, ThumbDir+"/") {
			continue
		}
		it, err := fs.Stat(ctx, p)
		if parent, _ := split(p); errors.Is(err, remote.ErrNotFound) && !listed[parent] {
			// Just stored by another device, perhaps.
			listed[parent] = true
			if _, _, err = fs.list(ctx, parent, false, true); err == nil {
				it, err = fs.Stat(ctx, p)
			}
		}
		if err != nil || it.IsDir {
			continue
		}
		if _, err := fs.Thumb(ctx, it); err == nil {
			continue
		}
		key := thumbKey(it)
		m.thumbMu.Lock()
		if !m.thumbSeen[key] && len(m.thumbWait) < thumbQueue {
			if m.thumbSeen == nil || len(m.thumbSeen) > 2*thumbQueue {
				m.thumbSeen = map[string]bool{}
				for _, w := range m.thumbWait {
					m.thumbSeen[thumbKey(w)] = true
				}
			}
			m.thumbSeen[key] = true
			m.thumbWait = append(m.thumbWait, it)
			queued++
			if !m.thumbBusy {
				m.thumbBusy = true
				go m.makeThumbs(fs)
			}
		}
		m.thumbMu.Unlock()
	}
	return queued
}

func (m *Manager) makeThumbs(fs *FS) {
	for {
		m.thumbMu.Lock()
		if len(m.thumbWait) == 0 {
			m.thumbBusy = false
			m.thumbMu.Unlock()
			return
		}
		it := m.thumbWait[0]
		m.thumbWait = m.thumbWait[1:]
		m.thumbMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), thumbLimit)
		data, err := remoteFrame(ctx, fs, it)
		if err == nil && data != nil {
			err = m.storeThumb(ctx, fs, it, data)
		}
		cancel()
		if err != nil {
			// It stays in thumbSeen: a file that gives no picture is not tried again.
			log.Printf("no thumbnail for %s: %v", it.Path, err)
			continue
		}
		m.thumbMu.Lock()
		delete(m.thumbSeen, thumbKey(it))
		m.thumbMu.Unlock()
	}
}

// smallBuffers keeps little data on its way to ffmpeg: what it has not read
// when it jumps elsewhere in the file was fetched from the remote for nothing.
type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetWriteBuffer(64 * 1024)
	}
	return c, err
}
