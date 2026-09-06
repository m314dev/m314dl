package mp4

import (
	"encoding/binary"
	"errors"
)

// SidxRef is one subsegment described by a SegmentIndexBox: its size in bytes
// and its duration in the sidx timescale.
type SidxRef struct {
	Size     uint32
	Duration uint32
}

// Sidx is a parsed SegmentIndexBox (ISO 14496-12 §8.16.3): the subsegments of
// an on-demand-profile DASH representation whose media is one big file.
type Sidx struct {
	Timescale uint64
	// FirstOffset is the byte distance from the end of the sidx box to the
	// first subsegment.
	FirstOffset uint64
	// End is the offset just past the sidx box within the parsed buffer, so the
	// first subsegment starts at (buffer start) + End + FirstOffset.
	End  int64
	Refs []SidxRef
}

// ParseSidx parses the first sidx box in buf (the bytes of a SegmentBase
// @indexRange). A hierarchical index (a sidx referencing further sidx boxes)
// is rejected: every reference must be to media.
func ParseSidx(buf []byte) (*Sidx, error) {
	sb, ok := find(buf, "sidx")
	if !ok {
		return nil, errors.New("mp4: no sidx box in index range")
	}
	version, _, rest, err := fullBox(sb.payload)
	if err != nil {
		return nil, err
	}
	// reference_ID(4) timescale(4) then EPT/first_offset: 4+4 (v0) or 8+8 (v1)
	need := 8 + 8
	if version != 0 {
		need = 8 + 16
	}
	if len(rest) < need+4 {
		return nil, errors.New("mp4: short sidx")
	}
	s := &Sidx{Timescale: uint64(binary.BigEndian.Uint32(rest[4:])), End: sb.off + sb.hdrLen + int64(len(sb.payload))}
	if version == 0 {
		s.FirstOffset = uint64(binary.BigEndian.Uint32(rest[12:]))
	} else {
		s.FirstOffset = binary.BigEndian.Uint64(rest[16:])
	}
	rest = rest[need:]
	count := int(binary.BigEndian.Uint16(rest[2:])) // reserved(2), reference_count(2)
	rest = rest[4:]
	if len(rest) < count*12 {
		return nil, errors.New("mp4: sidx truncated")
	}
	if s.Timescale == 0 {
		return nil, errors.New("mp4: sidx timescale 0")
	}
	s.Refs = make([]SidxRef, 0, count)
	for i := 0; i < count; i++ {
		e := rest[i*12:]
		sz := binary.BigEndian.Uint32(e)
		if sz&0x80000000 != 0 {
			return nil, errors.New("mp4: hierarchical sidx not supported")
		}
		s.Refs = append(s.Refs, SidxRef{Size: sz, Duration: binary.BigEndian.Uint32(e[4:])})
	}
	return s, nil
}
