package engine

import (
	"context"
	"encoding/binary"
	"io"
)

// splitTracksGap is how far apart the audio and the video of the same
// moment may be stored before a player reading one HTTP stream stalls;
// VLC buffers 16 MiB of a stream.
const splitTracksGap = 16 << 20

// SplitTracks reports whether it is an MP4 whose audio and video for the
// same moment are stored far apart, and how many audio tracks it has. A
// player then needs one stream per track: with a single one every switch
// between the tracks is a new request.
func (f *FS) SplitTracks(ctx context.Context, it *Item) (bool, int, error) {
	return mp4SplitTracks(it.Size, func(off, n int64) ([]byte, error) {
		rc, err := f.Open(ctx, it, off, n)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		b := make([]byte, n)
		_, err = io.ReadFull(rc, b)
		return b, err
	})
}

func mp4SplitTracks(size int64, read func(off, n int64) ([]byte, error)) (bool, int, error) {
	var moov []byte
	for off, i := int64(0), 0; moov == nil && off+16 <= size && i < 32; i++ {
		h, err := read(off, 16)
		if err != nil {
			return false, 0, err
		}
		n, hdr := int64(binary.BigEndian.Uint32(h)), int64(8)
		switch n {
		case 0:
			n = size - off
		case 1:
			n, hdr = int64(binary.BigEndian.Uint64(h[8:])), 16
		}
		if i == 0 && string(h[4:8]) != "ftyp" || n < hdr || n > size-off {
			return false, 0, nil
		}
		if string(h[4:8]) == "moov" {
			if n > 256<<20 {
				return false, 0, nil
			}
			if moov, err = read(off+hdr, n-hdr); err != nil {
				return false, 0, err
			}
		}
		off += n
	}
	var audio, video []int64
	tracks := 0
	mp4Boxes(moov, func(typ string, trak []byte) {
		if typ != "trak" {
			return
		}
		var handler string
		var offsets []int64
		mp4Boxes(mp4Box(trak, "mdia"), func(typ string, b []byte) {
			if typ == "hdlr" && len(b) >= 12 {
				handler = string(b[8:12])
			}
		})
		mp4Boxes(mp4Box(trak, "mdia", "minf", "stbl"), func(typ string, b []byte) {
			width := map[string]int{"stco": 4, "co64": 8}[typ]
			if width == 0 || len(b) < 8 {
				return
			}
			count := int(binary.BigEndian.Uint32(b[4:]))
			for b = b[8:]; count > 0 && len(b) >= width; count, b = count-1, b[width:] {
				if width == 4 {
					offsets = append(offsets, int64(binary.BigEndian.Uint32(b)))
				} else {
					offsets = append(offsets, int64(binary.BigEndian.Uint64(b)))
				}
			}
		})
		if handler == "soun" {
			tracks++
		}
		switch {
		case handler == "soun" && audio == nil:
			audio = offsets
		case handler == "vide" && video == nil:
			video = offsets
		}
	})
	if len(audio) == 0 || len(video) == 0 {
		return false, 0, nil
	}
	// Chunks of a track are about equally long, so the same share of each
	// table is about the same moment.
	for i := 1; i < 16; i++ {
		gap := audio[len(audio)*i/16] - video[len(video)*i/16]
		if gap > splitTracksGap || gap < -splitTracksGap {
			return true, tracks, nil
		}
	}
	return false, tracks, nil
}

// mp4Boxes calls fn with each box in b.
func mp4Boxes(b []byte, fn func(typ string, body []byte)) {
	for len(b) >= 8 {
		n, hdr := uint64(binary.BigEndian.Uint32(b)), uint64(8)
		if n == 1 && len(b) >= 16 {
			n, hdr = binary.BigEndian.Uint64(b[8:]), 16
		} else if n == 0 {
			n = uint64(len(b))
		}
		if n < hdr || n > uint64(len(b)) {
			return
		}
		fn(string(b[4:8]), b[hdr:n])
		b = b[n:]
	}
}

// mp4Box returns the body of the first box at path inside b.
func mp4Box(b []byte, path ...string) []byte {
	for _, p := range path {
		var found []byte
		mp4Boxes(b, func(typ string, body []byte) {
			if typ == p && found == nil {
				found = body
			}
		})
		b = found
	}
	return b
}
