// Package mp4 is a small reader for the ISO base media file format (ISO/IEC
// 14496-12), the box structure behind MP4 files. It does just enough to read
// CENC protection metadata (CENC is the common encryption standard for MP4,
// ISO/IEC 23001-7) and to decrypt fragmented MP4 samples inside this program,
// with no need for external tools such as mp4decrypt or shaka-packager.
package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// box is one parsed box: its header fields plus the raw payload. The payload
// is every byte after the header. For a container box it still holds the
// child boxes, unparsed.
type box struct {
	typ     string
	off     int64  // where the box starts (its size field) in the parsed buffer
	hdrLen  int64  // header length: size + type, plus largesize if present (8 or 16)
	payload []byte // bytes after the header; length = size - hdrLen
}

// containers lists the box types whose payload is itself a list of boxes.
var containers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"moof": true, "traf": true, "mvex": true, "edts": true, "dinf": true,
	"stsd": true, "sinf": true, "schi": true, "encv": true, "enca": true,
	"mp4a": true, "mp4v": true, "avc1": true, "hev1": true, "hvc1": true,
	"udta": true, "meta": true,
}

// sampleEntrySkips lists how many bytes a sample entry has before its child
// boxes begin: the 8-byte SampleEntry base plus a codec-specific block. The
// block is 70 bytes for visual entries, 20 for audio version 0 and 36 for
// audio version 1. We try each length and accept it only when the remaining
// bytes split exactly into boxes, so a wrong guess is rejected instead of
// being parsed as garbage.
var sampleEntrySkips = []int{8 + 70, 8 + 20, 8 + 36, 8 + 28, 8}

// walk calls fn for every top-level box in buf, in order. fn returns false to
// stop early.
func walk(buf []byte, fn func(b box) bool) error {
	var off int64
	for off < int64(len(buf)) {
		b, next, err := readBox(buf, off)
		if err != nil {
			return err
		}
		if !fn(b) {
			return nil
		}
		off = next
	}
	return nil
}

// readBox parses one box header at off and returns the box plus the offset of
// the following box.
func readBox(buf []byte, off int64) (box, int64, error) {
	if off < 0 || off > int64(len(buf))-8 {
		return box{}, 0, errors.New("mp4: truncated box header")
	}
	size := int64(binary.BigEndian.Uint32(buf[off:]))
	typ := string(buf[off+4 : off+8])
	hdr := int64(8)
	switch size {
	case 1: // size 1 means a 64-bit largesize field follows the type
		if off+16 > int64(len(buf)) {
			return box{}, 0, errors.New("mp4: truncated largesize")
		}
		size = int64(binary.BigEndian.Uint64(buf[off+8:]))
		hdr = 16
	case 0: // size 0 means the box runs to the end of the buffer
		size = int64(len(buf)) - off
	}
	if size < hdr || size > int64(len(buf))-off {
		return box{}, 0, fmt.Errorf("mp4: box %q size %d out of range at %d", typ, size, off)
	}
	return box{typ: typ, off: off, hdrLen: hdr, payload: buf[off+hdr : off+size]}, off + size, nil
}

// children calls fn for every child box inside a container box.
func (b box) children(fn func(c box) bool) error {
	return walk(b.payload, fn)
}

// find follows a path of box types and returns the box at its end.
// find(buf, "moov", "trak", "mdia") takes the first moov, then the first trak
// inside it, then the first mdia inside that.
func find(buf []byte, path ...string) (box, bool) {
	cur := buf
	var found box
	for depth, want := range path {
		got := false
		walk(cur, func(b box) bool {
			if b.typ == want {
				found = b
				got = true
				return false
			}
			return true
		})
		if !got {
			return box{}, false
		}
		if depth == len(path)-1 {
			return found, true
		}
		cur = found.payload
	}
	return box{}, false
}

// fullBox reads the version byte and the 3-byte flags that start a FullBox
// payload, and returns them along with the bytes that follow.
func fullBox(payload []byte) (version byte, flags uint32, rest []byte, err error) {
	if len(payload) < 4 {
		return 0, 0, nil, errors.New("mp4: truncated fullbox")
	}
	version = payload[0]
	flags = uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
	return version, flags, payload[4:], nil
}

// sampleEntryChildren returns the child boxes (such as sinf) of a sample entry
// box (encv/enca) by skipping the fixed prefix in front of them. The prefix
// length differs between visual and audio entries, so instead of hardcoding
// one length we try each known length and keep the first that lands on a
// real box boundary.
func sampleEntryChildren(b box) []byte {
	p := b.payload
	for _, skip := range sampleEntrySkips {
		if skip <= len(p) && tilesExactly(p[skip:]) && hasChild(p[skip:], "sinf") {
			return p[skip:]
		}
	}
	// No sinf found (a clear, unencrypted entry has none). Fall back to the
	// first length whose remainder splits exactly into boxes.
	for _, skip := range sampleEntrySkips {
		if skip <= len(p) && tilesExactly(p[skip:]) {
			return p[skip:]
		}
	}
	return nil
}

func hasChild(buf []byte, typ string) bool {
	found := false
	walk(buf, func(b box) bool {
		if b.typ == typ {
			found = true
			return false
		}
		return true
	})
	return found
}

// tilesExactly reports whether buf is a run of well-formed boxes that fills it
// exactly, with no bytes left over. That is a strong sign that buf starts at a
// real box boundary. An empty buf counts as tiling.
func tilesExactly(buf []byte) bool {
	off := 0
	for off < len(buf) {
		if off+8 > len(buf) {
			return false
		}
		size := int(binary.BigEndian.Uint32(buf[off:]))
		typ := string(buf[off+4 : off+8])
		if size == 1 {
			if off+16 > len(buf) {
				return false
			}
			size = int(binary.BigEndian.Uint64(buf[off+8:]))
		}
		if size < 8 || off+size > len(buf) || !isBoxType(typ) {
			return false
		}
		off += size
	}
	return off == len(buf)
}

func isBoxType(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == ' ') {
			return false
		}
	}
	return true
}
