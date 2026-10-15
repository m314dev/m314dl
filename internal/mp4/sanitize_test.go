package mp4

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestStripFragmentProtection(t *testing.T) {
	tfhd := mkFullBox("tfhd", 0, 0x020000, u32(1))
	trun := mkFullBox("trun", 0, 0x000001|0x000200, concat(u32(1), u32(100), u32(50)))
	senc := mkFullBox("senc", 0, 0x000002, concat(u32(1), make([]byte, 8), u16(1), u16(10), u32(40)))
	saiz := mkFullBox("saiz", 0, 0, concat([]byte{0}, u32(1), []byte{14}))
	saio := mkFullBox("saio", 0, 0, concat(u32(1), u32(200)))
	frag := concat(
		mkBox("moof", mkBox("traf", concat(tfhd, trun, senc, saiz, saio))),
		mkBox("mdat", make([]byte, 50)),
	)

	out := StripFragmentProtection(frag)
	if !tilesExactly(out) {
		t.Fatal("output is not a well-formed box sequence")
	}
	for _, typ := range []string{"senc", "saiz", "saio"} {
		if bytes.Contains(out, []byte(typ)) {
			t.Fatalf("%s box not stripped", typ)
		}
	}
	for _, typ := range []string{"moof", "traf", "tfhd", "trun", "mdat"} {
		if !bytes.Contains(out, []byte(typ)) {
			t.Fatalf("%s box lost", typ)
		}
	}
	if len(out) >= len(frag) {
		t.Fatal("nothing was removed")
	}

	// The regression: shrinking the moof shifts the mdat earlier, so trun's
	// data_offset must drop by exactly the bytes removed, or the demuxer reads
	// samples from the wrong place. Original data_offset here is 100 (set above).
	const origOff = 100
	var newOff int32
	walk(out, func(b box) bool {
		if b.typ == "moof" {
			if trun, ok := find(b.payload, "traf", "trun"); ok {
				newOff = int32(binary.BigEndian.Uint32(trun.payload[8:12]))
			}
		}
		return true
	})
	removed := len(frag) - len(out) // only senc/saiz/saio were dropped
	if int(origOff-newOff) != removed {
		t.Fatalf("trun data_offset dropped by %d, want %d (bytes removed from moof)", origOff-newOff, removed)
	}
}

func TestSanitizeInit(t *testing.T) {
	// sinf{frma='avc1', schm, schi{tenc}} inside an encv sample entry, plus a
	// sibling avcC (codec config) that must survive, and a moov-level pssh.
	sinf := mkBox("sinf", concat(
		mkBox("frma", []byte("avc1")),
		mkFullBox("schm", 0, 0, concat([]byte("cenc"), u32(0x10000))),
		mkBox("schi", mkFullBox("tenc", 0, 0, concat([]byte{0, 0, 1, 8}, make([]byte, 16)))),
	))
	avcC := mkBox("avcC", []byte{1, 2, 3, 4})
	encv := mkBox("encv", concat(make([]byte, 78), sinf, avcC)) // 78 = 8 base + 70 visual
	stsd := mkFullBox("stsd", 0, 0, concat(u32(1), encv))       // entry_count = 1
	moov := mkBox("moov", concat(
		mkBox("trak", mkBox("mdia", mkBox("minf", mkBox("stbl", stsd)))),
		mkFullBox("pssh", 0, 0, make([]byte, 20)),
	))
	init := concat(mkBox("ftyp", []byte("isom")), moov)

	out := SanitizeInit(init)
	if !tilesExactly(out) {
		t.Fatal("output is not a well-formed box sequence")
	}
	for _, typ := range []string{"encv", "sinf", "tenc", "schm", "pssh", "frma"} {
		if bytes.Contains(out, []byte(typ)) {
			t.Fatalf("%s should be gone after sanitize", typ)
		}
	}
	if !bytes.Contains(out, []byte("avc1")) {
		t.Fatal("original codec fourcc (avc1) not restored")
	}
	if !bytes.Contains(out, []byte("avcC")) {
		t.Fatal("avcC codec config lost")
	}
}

func TestSanitizeEAC3InitPreservesCodecConfig(t *testing.T) {
	// Encrypted EAC3 uses an enca entry just like encrypted AAC, but must be
	// restored to ec-3 with its dec3 decoder configuration intact.
	dec3 := mkBox("dec3", []byte{0x04, 0x00, 0x20, 0x0f, 0x00})
	sinf := mkBox("sinf", concat(
		mkBox("frma", []byte("ec-3")),
		mkFullBox("schm", 0, 0, concat([]byte("cenc"), u32(0x10000))),
		mkBox("schi", mkFullBox("tenc", 0, 0, concat([]byte{0, 0, 1, 8}, make([]byte, 16)))),
	))
	entry := mkBox("enca", concat(make([]byte, 28), dec3, sinf))
	stsd := mkFullBox("stsd", 0, 0, concat(u32(1), entry))
	init := mkBox("moov", mkBox("trak", mkBox("mdia", mkBox("minf", mkBox("stbl", stsd)))))
	info, err := ParseInit(init)
	if err != nil || info == nil || !info.Protected {
		t.Fatalf("EAC3 protection not recognized: %+v, %v", info, err)
	}
	out := SanitizeInit(init)
	stsdBox, ok := find(out, "moov", "trak", "mdia", "minf", "stbl", "stsd")
	if !ok {
		t.Fatal("sanitized init lost stsd")
	}
	clear, ok := find(stsdBox.payload[8:], "ec-3")
	if !ok || !bytes.Equal(clear.payload[28:], dec3) {
		t.Fatal("EAC3 codec or dec3 configuration was not preserved")
	}
	if info, err := ParseInit(out); err != nil || info != nil {
		t.Fatalf("sanitized EAC3 still encrypted: %+v, %v", info, err)
	}
}
