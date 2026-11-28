package engine

import (
	"encoding/binary"
	"testing"
)

func box(typ string, body ...[]byte) []byte {
	b := append(make([]byte, 4), typ...)
	for _, p := range body {
		b = append(b, p...)
	}
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	return b
}

func trak(handler string, offsets []int64, wide bool) []byte {
	typ, tbl := "stco", binary.BigEndian.AppendUint32(make([]byte, 4), uint32(len(offsets)))
	for _, o := range offsets {
		if wide {
			typ, tbl = "co64", binary.BigEndian.AppendUint64(tbl, uint64(o))
		} else {
			tbl = binary.BigEndian.AppendUint32(tbl, uint32(o))
		}
	}
	hdlr := append(make([]byte, 8), handler...)
	return box("trak", box("tkhd", make([]byte, 84)),
		box("mdia", box("hdlr", hdlr, make([]byte, 13)), box("minf", box("stbl", box(typ, tbl)))))
}

// mp4 is a file whose moov comes after a size bytes long mdat that is never
// read.
func mp4(t *testing.T, size int64, traks ...[]byte) (int64, func(off, n int64) ([]byte, error)) {
	head := box("ftyp", []byte("isom\x00\x00\x02\x00"))
	mdat := binary.BigEndian.AppendUint64([]byte("\x00\x00\x00\x01mdat"), uint64(size))
	tail := box("moov", append([][]byte{box("mvhd", make([]byte, 100))}, traks...)...)
	at := int64(len(head))
	return at + size + int64(len(tail)), func(off, n int64) ([]byte, error) {
		b := make([]byte, n)
		switch {
		case off < at:
			copy(b, head[off:])
			copy(b[at-off:], mdat)
		case off < at+16:
			copy(b, mdat[off-at:])
		case off >= at+size:
			copy(b, tail[off-at-size:])
		default:
			t.Errorf("read %d bytes of media data at %d", n, off)
		}
		return b, nil
	}
}

func TestMP4SplitTracks(t *testing.T) {
	const size = 6 << 30
	var video, near, half []int64
	for o := int64(1024); o < size; o += 4 << 20 {
		video = append(video, o)
		near = append(near, o+(3<<20))
		half = append(half, o/2)
	}
	for _, c := range []struct {
		name  string
		traks [][]byte
		want  bool
	}{
		{"interleaved", [][]byte{trak("vide", video, true), trak("soun", near, true)}, false},
		// The layout of the file that would not seek: audio at half the
		// offset of the video it belongs to.
		{"audio lags", [][]byte{trak("vide", video, true), trak("soun", half, true)}, true},
		{"audio at the end", [][]byte{trak("vide", video[:100], false), trak("soun", []int64{2 << 30, 3 << 30}, false)}, true},
		{"second audio ignored", [][]byte{trak("vide", video, true), trak("soun", near, true), trak("soun", half, true)}, false},
		{"video only", [][]byte{trak("vide", video, true)}, false},
	} {
		n, read := mp4(t, size, c.traks...)
		got, _, err := mp4SplitTracks(n, read)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v, want %v", c.name, got, err, c.want)
		}
	}
	got, _, err := mp4SplitTracks(1<<20, func(off, n int64) ([]byte, error) {
		return append([]byte("\x1aE\xdf\xa3"), make([]byte, n)...)[:n], nil
	})
	if got || err != nil {
		t.Errorf("not an mp4: got %v, %v", got, err)
	}
}
