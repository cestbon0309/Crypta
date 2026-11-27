package engine

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/cestbon0309/crypta/core/remote"
)

// Large files are stored the way rclone's chunker overlay stores them on top
// of the crypt remote (chunker -> crypt -> Drive), with chunker's defaults:
// the data is cut into "name.rclone_chunk.001", ".002", ... and a small JSON
// object under the plain name records size, chunk count and MD5. Every piece
// is an ordinary crypt file, so the pieces can be uploaded side by side.

// DefaultChunkSize is used when rclone.conf has no chunker remote.
const DefaultChunkSize = 256 * 1024 * 1024

// A metadata object is never larger than this (rclone's own limit).
const maxChunkMeta = 1023

var chunkName = regexp.MustCompile(`^(.+)\.rclone_chunk\.([0-9]{3,})(_[0-9a-z]{4,9})?$`)

func chunkFile(name string, no int) string {
	return fmt.Sprintf("%s.rclone_chunk.%03d", name, no+1)
}

// isChunk says whether name is a chunk (or a temporary chunk) of base.
func isChunk(name string) (base string, ok bool) {
	m := chunkName.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// joinChunks turns the entries of one directory into what the user sees:
// chunks are hidden, and a metadata object with a complete run of chunks
// becomes one file of their total size.
func joinChunks(items map[string]*Item) {
	parts := map[string]map[int]*Item{}
	for name, it := range items {
		if it.IsDir {
			continue
		}
		m := chunkName.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		delete(items, name)
		if m[3] != "" { // temporary chunk of an upload by rclone
			continue
		}
		no, err := strconv.Atoi(m[2])
		if err != nil || no < 1 {
			continue
		}
		if parts[m[1]] == nil {
			parts[m[1]] = map[int]*Item{}
		}
		parts[m[1]][no] = it
	}
	for base, byNo := range parts {
		meta := items[base]
		if meta == nil || meta.IsDir || meta.Size > maxChunkMeta {
			continue // chunks without their metadata are not a file (yet)
		}
		list := make([]*Item, 0, len(byNo))
		var size int64
		for no := 1; byNo[no] != nil; no++ {
			list = append(list, byNo[no])
			size += byNo[no].Size
		}
		if len(list) != len(byNo) {
			continue
		}
		meta.parts, meta.Size = list, size
	}
}

// dropChunks deletes every chunk of name in a directory: what an upload
// that was given up left behind.
func (f *FS) dropChunks(ctx context.Context, dirID, name string) error {
	entries, err := f.R.List(ctx, dirID)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		plain, err := f.C.DecryptName(e.Name, false)
		if err != nil {
			continue
		}
		if base, ok := isChunk(plain); ok && base == name {
			if err := f.R.Delete(ctx, e.ID); err != nil && err != remote.ErrNotFound {
				return err
			}
		}
	}
	return nil
}

// dropChunksAt is dropChunks for a full path.
func (f *FS) dropChunksAt(ctx context.Context, p string) error {
	parent, name := split(Clean(p))
	dc, err := f.dir(ctx, parent, false)
	if err != nil {
		return err
	}
	return f.dropChunks(ctx, dc.id, name)
}

// partsReader reads a range of a chunked file, one chunk after the other.
type partsReader struct {
	f     *FS
	ctx   context.Context
	parts []*Item
	off   int64 // offset inside parts[0]
	left  int64
	cur   io.ReadCloser
}

func (p *partsReader) Read(b []byte) (int, error) {
	for {
		if p.left <= 0 {
			return 0, io.EOF
		}
		if p.cur == nil {
			for len(p.parts) > 0 && p.off >= p.parts[0].Size {
				p.off -= p.parts[0].Size
				p.parts = p.parts[1:]
			}
			if len(p.parts) == 0 {
				return 0, io.ErrUnexpectedEOF
			}
			rc, err := p.f.openPart(p.ctx, p.parts[0], p.off, min(p.left, p.parts[0].Size-p.off))
			if err != nil {
				return 0, err
			}
			p.cur = rc
		}
		if int64(len(b)) > p.left {
			b = b[:p.left]
		}
		n, err := p.cur.Read(b)
		p.off += int64(n)
		p.left -= int64(n)
		if err == io.EOF {
			p.cur.Close()
			p.cur, err = nil, nil
			if p.off < p.parts[0].Size && p.left > 0 {
				return n, io.ErrUnexpectedEOF
			}
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
}

func (p *partsReader) Close() error {
	if p.cur != nil {
		return p.cur.Close()
	}
	return nil
}

// parseSize reads an rclone size such as "256M", "2Gi" or "100" (KiB).
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	num := strings.TrimRight(s, "BbKkMmGgTtPpi")
	unit := strings.ToUpper(strings.TrimSuffix(strings.TrimSuffix(s[len(num):], "i"), "B"))
	if s[len(num):] == "B" || s[len(num):] == "b" {
		unit = "B"
	}
	mult := map[string]float64{"B": 1, "": 1 << 10, "K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40, "P": 1 << 50}[unit]
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || mult == 0 || v <= 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return int64(v * mult), nil
}

// chunkerOf finds the chunker remote that wraps the crypt remote and returns
// its chunk size, or the default when there is none.
func chunkerOf(secs map[string]iniSection, crypt string) (int64, error) {
	for name, s := range secs {
		if s["type"] != "chunker" || strings.TrimSuffix(s["remote"], "/") != crypt+":" {
			continue
		}
		for key, want := range map[string]string{"name_format": "*.rclone_chunk.###", "meta_format": "simplejson", "start_from": "1", "transactions": "rename"} {
			if v := s[key]; v != "" && v != want {
				return 0, fmt.Errorf("chunker remote %q: %s = %s is not supported, only the default (%s)", name, key, v, want)
			}
		}
		if s["chunk_size"] == "" {
			return 2 << 30, nil // rclone's default
		}
		size, err := parseSize(s["chunk_size"])
		if err != nil {
			return 0, fmt.Errorf("chunker remote %q: %w", name, err)
		}
		if size < 1<<20 {
			return 0, fmt.Errorf("chunker remote %q: chunk_size below 1M is not supported", name)
		}
		return size, nil
	}
	return 0, nil
}
