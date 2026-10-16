package subs

import (
	"encoding/binary"
	"math"
	"reflect"
	"testing"

	"github.com/m314dev/m314dl/internal/mp4"
)

func word(n uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, n); return b }
func full(typ string, version byte, flags uint32, parts ...[]byte) []byte {
	return mp4.Box(typ, append([][]byte{{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}}, parts...)...)
}
func wvttInit(scale uint32) []byte {
	tkhd := full("tkhd", 0, 0, word(0), word(0), word(7))
	mdhd := full("mdhd", 0, 0, word(0), word(0), word(scale))
	stsd := full("stsd", 0, 0, word(1), mp4.Box("wvtt", make([]byte, 8), mp4.Box("vttC", []byte("WEBVTT"))))
	return mp4.Box("moov", mp4.Box("trak", tkhd, mp4.Box("mdia", mdhd, mp4.Box("minf", mp4.Box("stbl", stsd)))))
}
func wvttFragment(start uint64, durations []uint32, samples ...[]byte) []byte {
	time := make([]byte, 8)
	binary.BigEndian.PutUint64(time, start)
	entries := [][]byte{word(uint32(len(samples))), word(0)}
	for i, sample := range samples {
		entries = append(entries, word(durations[i]), word(uint32(len(sample))))
	}
	makeMoof := func() []byte {
		return mp4.Box("moof", mp4.Box("traf",
			full("tfhd", 0, 0x020000, word(7)), full("tfdt", 1, 0, time), full("trun", 0, 0x301, entries...)))
	}
	entries[1] = word(uint32(len(makeMoof()) + 8))
	return append(makeMoof(), mp4.Box("mdat", samples...)...)
}
func cue(text string) []byte {
	return mp4.Box("vttc", mp4.Box("sttg", []byte("align:start")), mp4.Box("iden", []byte(text)),
		mp4.Box("ctim", []byte("00:00:00.000")), mp4.Box("payl", []byte(text)))
}

func TestWVTTTimingGapsAndOverlappingCues(t *testing.T) {
	b := wvttInit(90000)
	b = append(b, wvttFragment(0, []uint32{90000, 180000, 90000}, mp4.Box("vtte"), cue("A"), append(cue("A"), cue("B")...))...)
	b = append(b, wvttFragment(360000, []uint32{180000, 4500, 90000}, cue("B"), mp4.Box("vtte"), cue("B"))...)
	got, err := ParseWVTT(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []Cue{{Start: 1, End: 4, Text: "A"}, {Start: 3, End: 6, Text: "B"}, {Start: 6.05, End: 7.05, Text: "B"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cues = %+v, want %+v", got, want)
	}
}

func TestWVTTUnicodeAndLargeDecodeTime(t *testing.T) {
	start := uint64(1) << 33
	b := append(wvttInit(1000), wvttFragment(start, []uint32{1250}, cue("<c.white>مرحبا</c> &amp; hello\nworld"))...)
	got, err := ParseWVTT(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "مرحبا & hello\nworld" || math.Abs(got[0].Start-float64(start)/1000) > 1e-6 || math.Abs(got[0].End-got[0].Start-1.25) > 1e-6 {
		t.Fatalf("cues = %+v", got)
	}
}

func TestWVTTRejectsMalformedInput(t *testing.T) {
	valid := append(wvttInit(1000), wvttFragment(0, []uint32{1000}, cue("hello"))...)
	for _, b := range [][]byte{
		valid[:len(valid)-1],
		append(wvttInit(0), wvttFragment(0, []uint32{1000}, cue("hello"))...),
		append(wvttInit(1000), wvttFragment(0, []uint32{1000}, mp4.Box("vttc", mp4.Box("sttg")))...),
		append(wvttInit(1000), wvttFragment(0, []uint32{1000}, cue(string([]byte{0xff})))...),
	} {
		if _, err := ParseWVTT(b); err == nil {
			t.Fatal("malformed input accepted")
		}
	}
}

func FuzzParseWVTT(f *testing.F) {
	f.Add(append(wvttInit(1000), wvttFragment(0, []uint32{1000}, cue("hello"))...))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = ParseWVTT(b) })
}
