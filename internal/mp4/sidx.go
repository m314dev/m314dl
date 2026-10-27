package mp4

import (
	"encoding/binary"
	"errors"
)

// SidxRef is one subsegment listed in a SegmentIndexBox (sidx): its size in
// bytes and its duration in sidx timescale units.
type SidxRef struct {
	Size     uint32
	Duration uint32
}

// Sidx is a parsed SegmentIndexBox (ISO 14496-12 §8.16.3). It lists the
// subsegments of a DASH on-demand representation, where the whole media is one
// big file and the index says how large each piece is.
type Sidx struct {
	Timescale uint64
	// FirstOffset is the number of bytes between the end of the sidx box and
	// the first subsegment.
	FirstOffset uint64
	// End is the offset of the first byte after the sidx box, within the parsed
	// buffer. The first subsegment starts at End + FirstOffset, counted from
	// the start of that buffer.
	End  int64
	Refs []SidxRef
}

// ParseSidx parses the first sidx box in buf, which holds the bytes of a
// SegmentBase @indexRange. A hierarchical index (a sidx that points at further
// sidx boxes) is rejected: every reference must point at media.
func ParseSidx(buf []byte) (*Sidx, error) {
	sb, ok := find(buf, "sidx")
	if !ok {
		return nil, errors.New("mp4: no sidx box in index range")
	}
	version, _, rest, err := fullBox(sb.payload)
	if err != nil {
		return nil, err
	}
	// Layout: reference_ID(4), timescale(4), then earliest_presentation_time
	// (EPT) and first_offset: 4+4 bytes in version 0, 8+8 in version 1.
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
