package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// ErrTrackNotFound means the init has no sample entry for the requested codec.
var ErrTrackNotFound = errors.New("mp4: requested sample entry not found")

// TimedSample points at one unencrypted sample in the input buffer. Start and
// End are seconds on the media timeline, with any trun composition offset
// already applied.
type TimedSample struct {
	Start, End float64
	Data       []byte
}

type sampleDefaults struct{ description, duration, size uint32 }
type sampleTrack struct {
	id, timescale uint32
	descriptions  map[uint32]bool
	defaults      sampleDefaults
}

func boxes(buf []byte) ([]box, error) {
	var result []box
	err := walk(buf, func(b box) bool { result = append(result, b); return true })
	return result, err
}

// WalkSamples visits every sample of one codec's track in a buffer that holds
// an init segment followed by its fragments. It handles default values from
// tfhd and trex, several truns per traf, signed data offsets, and both tfdt
// versions (v0 and v1). A sample must lie inside an mdat box; one that would
// land in a neighbouring header is an error. No media bytes are copied. Flat
// (non-fragmented) MP4 with an stbl sample table is not supported, on purpose.
func WalkSamples(data []byte, codec string, yield func(TimedSample) error) error {
	top, err := boxes(data)
	if err != nil {
		return err
	}
	var track *sampleTrack
	var mdats []box
	for _, b := range top {
		if b.typ == "moov" && track == nil {
			track, err = findSampleTrack(b.payload, codec)
			if err != nil && !errors.Is(err, ErrTrackNotFound) {
				return err
			}
		}
		if b.typ == "mdat" {
			mdats = append(mdats, b)
		}
	}
	if track == nil {
		return ErrTrackNotFound
	}
	var decodeTime uint64
	haveTime, haveFragment := false, false
	for _, moof := range top {
		if moof.typ != "moof" {
			continue
		}
		children, err := boxes(moof.payload)
		if err != nil {
			return err
		}
		firstTraf := true
		for _, traf := range children {
			if traf.typ != "traf" {
				continue
			}
			isFirst := firstTraf
			firstTraf = false
			tfhd, ok := find(traf.payload, "tfhd")
			if !ok || len(tfhd.payload) < 8 {
				return fmt.Errorf("mp4: missing or short tfhd")
			}
			if binary.BigEndian.Uint32(tfhd.payload[4:]) != track.id {
				continue
			}
			haveFragment = true
			_, flags, rest, _ := fullBox(tfhd.payload)
			r := sampleReader{b: rest[4:]}
			base := moof.off
			if flags&1 != 0 {
				v := r.u64()
				if v > math.MaxInt64 {
					return fmt.Errorf("mp4: base data offset overflow")
				}
				base = int64(v)
			} else if flags&0x020000 == 0 && !isFirst {
				return fmt.Errorf("mp4: implicit data base after another track is unsupported")
			}
			defaults := track.defaults
			if flags&2 != 0 {
				defaults.description = r.u32()
			}
			if flags&8 != 0 {
				defaults.duration = r.u32()
			}
			if flags&16 != 0 {
				defaults.size = r.u32()
			}
			if flags&32 != 0 {
				r.u32()
			} // default sample flags
			if r.err != nil {
				return r.err
			}
			if !track.descriptions[defaults.description] {
				return fmt.Errorf("mp4: fragment switches away from %s", codec)
			}
			parts, err := boxes(traf.payload)
			if err != nil {
				return err
			}
			for _, part := range parts {
				if part.typ != "tfdt" {
					continue
				}
				version, _, rest, err := fullBox(part.payload)
				if err != nil {
					return err
				}
				r := sampleReader{b: rest}
				switch version {
				case 0:
					decodeTime = uint64(r.u32())
				case 1:
					decodeTime = r.u64()
				default:
					return fmt.Errorf("mp4: unsupported tfdt version %d", version)
				}
				if r.err != nil {
					return r.err
				}
				haveTime = true
			}
			if !haveTime {
				return fmt.Errorf("mp4: first %s fragment has no tfdt", codec)
			}
			pos := base
			for _, part := range parts {
				if part.typ != "trun" {
					continue
				}
				if err := walkSampleRun(data, mdats, part, base, &pos, &decodeTime, defaults, track.timescale, yield); err != nil {
					return err
				}
			}
		}
	}
	if !haveFragment {
		return fmt.Errorf("mp4: no fragments for %s track", codec)
	}
	return nil
}

func findSampleTrack(moov []byte, codec string) (*sampleTrack, error) {
	children, err := boxes(moov)
	if err != nil {
		return nil, err
	}
	var result *sampleTrack
	for _, trak := range children {
		if trak.typ != "trak" {
			continue
		}
		stsd, ok := find(trak.payload, "mdia", "minf", "stbl", "stsd")
		if !ok || len(stsd.payload) < 8 {
			continue
		}
		entries, err := boxes(stsd.payload[8:])
		if err != nil {
			return nil, err
		}
		descriptions := map[uint32]bool{}
		for i, entry := range entries {
			if entry.typ == codec {
				descriptions[uint32(i+1)] = true
			}
		}
		if len(descriptions) == 0 {
			continue
		}
		if result != nil {
			return nil, fmt.Errorf("mp4: multiple %s tracks", codec)
		}
		tkhd, haveID := find(trak.payload, "tkhd")
		mdhd, haveScale := find(trak.payload, "mdia", "mdhd")
		if !haveID || !haveScale {
			return nil, fmt.Errorf("mp4: missing tkhd/mdhd for %s", codec)
		}
		id, err := timeHeaderField(tkhd.payload)
		if err != nil {
			return nil, err
		}
		scale, err := timeHeaderField(mdhd.payload)
		if err != nil {
			return nil, err
		}
		if id == 0 || scale == 0 {
			return nil, fmt.Errorf("mp4: zero track ID or timescale")
		}
		result = &sampleTrack{id: id, timescale: scale, descriptions: descriptions, defaults: sampleDefaults{description: 1}}
	}
	if result == nil {
		return nil, ErrTrackNotFound
	}
	if mvex, ok := find(moov, "mvex"); ok {
		entries, err := boxes(mvex.payload)
		if err != nil {
			return nil, err
		}
		for _, b := range entries {
			if b.typ != "trex" {
				continue
			}
			if len(b.payload) < 24 {
				return nil, fmt.Errorf("mp4: short trex")
			}
			if binary.BigEndian.Uint32(b.payload[4:]) == result.id {
				result.defaults = sampleDefaults{
					description: binary.BigEndian.Uint32(b.payload[8:]),
					duration:    binary.BigEndian.Uint32(b.payload[12:]),
					size:        binary.BigEndian.Uint32(b.payload[16:]),
				}
			}
		}
	}
	return result, nil
}

// timeHeaderField reads the 32-bit field that comes right after the creation
// and modification times. In a tkhd that field is track_ID; in an mdhd it is
// timescale.
func timeHeaderField(payload []byte) (uint32, error) {
	v, _, rest, err := fullBox(payload)
	if err != nil {
		return 0, err
	}
	pos := 8
	if v == 1 {
		pos = 16
	} else if v != 0 {
		return 0, fmt.Errorf("mp4: unsupported time header version")
	}
	if len(rest) < pos+4 {
		return 0, fmt.Errorf("mp4: short time header")
	}
	return binary.BigEndian.Uint32(rest[pos:]), nil
}

func walkSampleRun(data []byte, mdats []box, trun box, base int64, pos *int64, dts *uint64,
	d sampleDefaults, scale uint32, yield func(TimedSample) error) error {
	v, flags, rest, err := fullBox(trun.payload)
	if err != nil {
		return err
	}
	if v > 1 {
		return fmt.Errorf("mp4: unsupported trun version %d", v)
	}
	r := sampleReader{b: rest}
	count := r.u32()
	if flags&1 != 0 {
		offset := int64(int32(r.u32()))
		if offset > 0 && base > math.MaxInt64-offset {
			return fmt.Errorf("mp4: data offset overflow")
		}
		*pos = base + offset
	}
	if flags&4 != 0 {
		r.u32()
	} // first sample flags
	if r.err != nil {
		return r.err
	}
	// Every sample we accept takes at least one byte, so the count can never be
	// larger than the input. This also caps the work done for a corrupt count
	// when sizes come from defaults and the trun has no per-sample fields.
	if uint64(count) > uint64(len(data)) {
		return fmt.Errorf("mp4: unreasonable sample count")
	}
	for i := uint32(0); i < count; i++ {
		duration, size := d.duration, d.size
		if flags&0x100 != 0 {
			duration = r.u32()
		}
		if flags&0x200 != 0 {
			size = r.u32()
		}
		if flags&0x400 != 0 {
			r.u32()
		}
		var cto int64
		if flags&0x800 != 0 {
			u := r.u32()
			cto = int64(u)
			if v == 1 {
				cto = int64(int32(u))
			}
		}
		if r.err != nil {
			return r.err
		}
		if size == 0 || duration == 0 {
			return fmt.Errorf("mp4: missing sample size or duration")
		}
		if *pos < 0 || *pos > int64(len(data))-int64(size) {
			return fmt.Errorf("mp4: sample outside input")
		}
		end := *pos + int64(size)
		// mdat boxes are sorted by file offset, so a binary search finds the one
		// that may hold this sample. Scanning every earlier fragment for every
		// sample would be slow on subtitle tracks with thousands of parts.
		idx := sort.Search(len(mdats), func(i int) bool { return mdats[i].off > *pos }) - 1
		inside := idx >= 0 && *pos >= mdats[idx].off+mdats[idx].hdrLen && end <= mdats[idx].off+mdats[idx].hdrLen+int64(len(mdats[idx].payload))
		if !inside {
			return fmt.Errorf("mp4: sample outside mdat")
		}
		if *dts > math.MaxUint64-uint64(duration) {
			return fmt.Errorf("mp4: sample time overflow")
		}
		start := (float64(*dts) + float64(cto)) / float64(scale)
		if err := yield(TimedSample{Start: start, End: start + float64(duration)/float64(scale), Data: data[*pos:end]}); err != nil {
			return err
		}
		*dts += uint64(duration)
		*pos = end
	}
	return nil
}

type sampleReader struct {
	b   []byte
	err error
}

func (r *sampleReader) u32() uint32 {
	if len(r.b) < 4 {
		r.err = fmt.Errorf("mp4: truncated sample metadata")
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}
func (r *sampleReader) u64() uint64 { hi, lo := r.u32(), r.u32(); return uint64(hi)<<32 | uint64(lo) }

// WalkBoxes calls yield with the type and payload of each top-level box in
// data, without copying. Codec-specific code uses it to read sample boxes.
func WalkBoxes(data []byte, yield func(typ string, payload []byte) error) error {
	var visitErr error
	err := walk(data, func(b box) bool { visitErr = yield(b.typ, b.payload); return visitErr == nil })
	if err != nil {
		return err
	}
	return visitErr
}
