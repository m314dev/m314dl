package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"testing"
)

func sampleInit(version byte, duration, size uint32) []byte {
	times := make([]byte, 8)
	if version == 1 {
		times = make([]byte, 16)
	}
	trak := Box("trak", mkFullBox("tkhd", version, 0, concat(times, u32(9))),
		Box("mdia", mkFullBox("mdhd", version, 0, concat(times, u32(1000))),
			Box("minf", Box("stbl", mkFullBox("stsd", 0, 0, concat(u32(1), Box("wvtt", make([]byte, 8))))))))
	return Box("moov", trak, Box("mvex", mkFullBox("trex", 0, 0, concat(u32(9), u32(1), u32(duration), u32(size), u32(0)))))
}

func collectSamples(b []byte) ([]TimedSample, error) {
	var out []TimedSample
	err := WalkSamples(b, "wvtt", func(s TimedSample) error { out = append(out, s); return nil })
	return out, err
}

func TestWalkSamplesDefaultsAndMultipleRuns(t *testing.T) {
	for _, version := range []byte{0, 1} {
		init := sampleInit(version, 500, 3)
		tfhd := mkFullBox("tfhd", 0, 0x020000|8|16|32, concat(u32(9), u32(1000), u32(2), u32(0)))
		time := u32(2000)
		if version == 1 {
			time = be64(2000)
		}
		makeMoof := func(offset uint32) []byte {
			return Box("moof", Box("traf", tfhd,
				mkFullBox("tfdt", version, 0, time),
				mkFullBox("trun", 0, 1|4, concat(u32(2), u32(offset), u32(0))),
				// second run inherits its data position and overrides sample timing/size
				mkFullBox("trun", 1, 0x100|0x200|0x400|0x800, concat(u32(1), u32(500), u32(3), u32(0), u32(0xffffff9c)))))
		}
		moof := makeMoof(0)
		moof = makeMoof(uint32(len(moof) + 8))
		got, err := collectSamples(concat(init, moof, Box("mdat", []byte("aabbccc"))))
		if err != nil {
			t.Fatal(err)
		}
		want := []TimedSample{{2, 3, []byte("aa")}, {3, 4, []byte("bb")}, {3.9, 4.4, []byte("ccc")}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("v%d samples = %+v, want %+v", version, got, want)
		}
	}
}

func TestWalkSamplesTrexAndAbsoluteBase(t *testing.T) {
	init := sampleInit(0, 750, 2)
	makeMoof := func(base uint64) []byte {
		return Box("moof", Box("traf",
			mkFullBox("tfhd", 0, 1|2, concat(u32(9), be64(base), u32(1))),
			mkFullBox("tfdt", 0, 0, u32(1000)),
			mkFullBox("trun", 0, 1, concat(u32(2), u32(0xfffffffe)))))
	}
	moof := makeMoof(0)
	moof = makeMoof(uint64(len(init) + len(moof) + 8 + 2)) // signed -2 offset lands on mdat
	got, err := collectSamples(concat(init, moof, Box("mdat", []byte("aabb"))))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Start != 1 || got[1].End != 2.5 || string(got[1].Data) != "bb" {
		t.Fatalf("samples = %+v", got)
	}
}

func TestWalkSamplesExplicitRunOffsets(t *testing.T) {
	init := sampleInit(0, 1000, 2)
	makeMoof := func(first, second uint32) []byte {
		return Box("moof", Box("traf",
			mkFullBox("tfhd", 0, 0x020000, u32(9)), mkFullBox("tfdt", 0, 0, u32(0)),
			mkFullBox("trun", 0, 1, concat(u32(1), u32(first))),
			mkFullBox("trun", 0, 1, concat(u32(1), u32(second)))))
	}
	moof := makeMoof(0, 0)
	moof = makeMoof(uint32(len(moof)+8), uint32(len(moof)+8+4))
	got, err := collectSamples(concat(init, moof, Box("mdat", []byte("aaXXbb"))))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got[1].Data) != "bb" {
		t.Fatalf("samples = %+v", got)
	}
}

func TestWalkSamplesErrors(t *testing.T) {
	init := sampleInit(0, 1000, 2)
	moof := Box("moof", Box("traf", mkFullBox("tfhd", 0, 0x020000, u32(9)),
		mkFullBox("tfdt", 0, 0, u32(0)), mkFullBox("trun", 0, 1, concat(u32(1), u32(0)))))
	for _, tc := range []struct {
		name   string
		offset uint32
	}{
		{"header", 0}, {"past input", math.MaxInt32}, {"negative", 0x80000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := append([]byte(nil), moof...)
			pos := bytes.Index(b, []byte("trun"))
			binary.BigEndian.PutUint32(b[pos+12:], tc.offset)
			if _, err := collectSamples(concat(init, b, Box("mdat", []byte("aa")))); err == nil {
				t.Fatal("invalid offset accepted")
			}
		})
	}
	if _, err := collectSamples(Box("moov")); !errors.Is(err, ErrTrackNotFound) {
		t.Fatalf("no track: %v", err)
	}
	if _, err := collectSamples(init); err == nil {
		t.Fatal("no fragments accepted")
	}
	// An overflowing largesize must return an error, not panic while slicing.
	bad := concat(Box("free"), u32(1), []byte("mdat"), be64(math.MaxInt64))
	if _, err := collectSamples(bad); err == nil {
		t.Fatal("overflowing box accepted")
	}
}
