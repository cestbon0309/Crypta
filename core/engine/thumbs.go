package engine

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
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
	items, _, err := f.list(ctx, ThumbDir, false, false)
	if err != nil {
		return nil, err
	}
	key := thumbKey(it)
	for name, th := range items {
		if !th.IsDir && strings.HasPrefix(name, key) {
			return th, nil
		}
	}
	return nil, fmt.Errorf("thumbnail of %s: %w", it.Path, remote.ErrNotFound)
}

// frame makes the thumbnail of a local picture or video with ffmpeg; nil
// when the file is neither or there is no ffmpeg.
func frame(ctx context.Context, file, name string) ([]byte, error) {
	ext := strings.ToLower(path.Ext(name))
	if !imageExts[ext] && !videoExts[ext] {
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
	said, _, _ := strings.Cut(strings.TrimSpace(msg.String()), "\n")
	if err := cmd.Run(); err != nil {
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
