package mp4

import "testing"

// buildSidx assembles a version-0 SegmentIndexBox with the given references.
func buildSidx(timescale, firstOffset uint32, refs []SidxRef) []byte {
	body := concatBytes(fullBoxHdr(0, 0), be32(1), be32(timescale), be32(0), be32(firstOffset), be16(0), be16(uint16(len(refs))))
	for _, r := range refs {
		body = append(body, concatBytes(be32(r.Size), be32(r.Duration), be32(0x90000000))...)
	}
	return Box("sidx", body)
}

func TestParseSidx(t *testing.T) {
	want := []SidxRef{{Size: 1000, Duration: 48000}, {Size: 2000, Duration: 48000}, {Size: 500, Duration: 24000}}
	buf := buildSidx(48000, 0, want)
	s, err := ParseSidx(buf)
	if err != nil {
		t.Fatal(err)
	}
	if s.Timescale != 48000 || s.FirstOffset != 0 || s.End != int64(len(buf)) {
		t.Fatalf("header = %+v (len %d)", s, len(buf))
	}
	if len(s.Refs) != len(want) {
		t.Fatalf("refs = %d", len(s.Refs))
	}
	for i := range want {
		if s.Refs[i] != want[i] {
			t.Fatalf("ref %d = %+v, want %+v", i, s.Refs[i], want[i])
		}
	}
	// a leading unrelated box (styp) before the sidx is skipped
	if s2, err := ParseSidx(append(Box("styp", []byte("dash")), buf...)); err != nil || s2.End != int64(len(buf))+12 {
		t.Fatalf("sidx after styp: %v %+v", err, s2)
	}
	// hierarchical references are refused rather than misread as media
	if _, err := ParseSidx(buildSidx(48000, 0, []SidxRef{{Size: 0x80000010, Duration: 1}})); err == nil {
		t.Fatal("hierarchical sidx should be rejected")
	}
	if _, err := ParseSidx(Box("moof", nil)); err == nil {
		t.Fatal("missing sidx should error")
	}
}
