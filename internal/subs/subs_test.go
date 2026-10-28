package subs

import (
	"encoding/binary"
	"strings"
	"testing"
)

const vttConcat = `WEBVTT

00:00:01.000 --> 00:00:03.000
hello

00:00:03.000 --> 00:00:05.000
world

WEBVTT

00:00:03.000 --> 00:00:05.000
world

00:00:05.000 --> 00:00:07.000
again
`

func TestVTTConcatDedupe(t *testing.T) {
	cues := parseVTT(vttConcat)
	if len(cues) != 3 {
		t.Fatalf("cues = %d, want 3 (overlap deduped): %+v", len(cues), cues)
	}
	if cues[0].Text != "hello" || cues[2].Text != "again" {
		t.Fatalf("cues = %+v", cues)
	}
}

func TestVTTStripsTagsAndEntities(t *testing.T) {
	// OSN uses <c.white> cue tags; dsnp_ar uses the &rlm; entity. Both must be
	// gone.
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\n<c.white>hello</c>\n\n" +
		"00:00:03.000 --> 00:00:04.000\n&rlm;مرحبا &amp; more\n"
	cues := parseVTT(vtt)
	if len(cues) != 2 {
		t.Fatalf("cues = %d, want 2: %+v", len(cues), cues)
	}
	if cues[0].Text != "hello" {
		t.Fatalf("tag not stripped: %q", cues[0].Text)
	}
	if strings.Contains(cues[1].Text, "&rlm;") || strings.Contains(cues[1].Text, "&amp;") {
		t.Fatalf("entities not decoded: %q", cues[1].Text)
	}
	if cues[1].Text != "‏مرحبا & more" { // &rlm; decodes to U+200F and &amp; to &
		t.Fatalf("cue1 = %q", cues[1].Text)
	}
}

const ttml = `<?xml version="1.0"?>
<tt xmlns="http://www.w3.org/ns/ttml"><body><div>
<p begin="00:00:01.500" end="00:00:03.000">line one<br/>line two</p>
<p begin="4.5s" end="6s"><span>styled</span> text &amp; more</p>
</div></body></tt>`

func TestTTML(t *testing.T) {
	cues := parseTTML(ttml)
	if len(cues) != 2 {
		t.Fatalf("cues = %d", len(cues))
	}
	if cues[0].Start != 1.5 || cues[0].Text != "line one\nline two" {
		t.Fatalf("c0 = %+v", cues[0])
	}
	if cues[1].Start != 4.5 || cues[1].Text != "styled text & more" {
		t.Fatalf("c1 = %+v", cues[1])
	}
}

// Broken XML (unclosed tags) must still yield cues; the parser is lenient by
// design.
func TestTTMLLenient(t *testing.T) {
	broken := `<tt><body><p begin="1s" end="2s">ok</p><p begin="3s">no end attr` // malformed tail
	cues := parseTTML(broken)
	if len(cues) != 1 || cues[0].Text != "ok" {
		t.Fatalf("cues = %+v", cues)
	}
}

func TestSniff(t *testing.T) {
	if Sniff([]byte("WEBVTT\n\n00:00:01.000 --> 2")) != KindVTT {
		t.Fatal("vtt")
	}
	if Sniff([]byte(`<?xml version="1.0"?><tt>`)) != KindTTML {
		t.Fatal("ttml")
	}
	box := make([]byte, 16)
	binary.BigEndian.PutUint32(box, 16)
	copy(box[4:8], "styp")
	if Sniff(box) != KindFMP4 {
		t.Fatal("fmp4")
	}
}

func TestExtractMdat(t *testing.T) {
	payload := []byte("<tt><p begin=\"1s\" end=\"2s\">x</p></tt>")
	box := make([]byte, 8+len(payload)+8)
	binary.BigEndian.PutUint32(box, 8)
	copy(box[4:8], "styp")
	binary.BigEndian.PutUint32(box[8:], uint32(8+len(payload)))
	copy(box[12:16], "mdat")
	copy(box[16:], payload)
	got := ExtractMdat(box[:8+8+len(payload)])
	if string(got) != string(payload) {
		t.Fatalf("mdat = %q", got)
	}
	if !IsTTMLPayload(got) {
		t.Fatal("should look like TTML")
	}
}

func TestSRTFormat(t *testing.T) {
	if s := fmtSRTTime(3723.5); s != "01:02:03,500" {
		t.Fatalf("srt time = %s", s)
	}
}

// Two HLS WebVTT segments whose cue times restart at zero in each segment. The
// X-TIMESTAMP-MAP header carries the real position: segment two starts ten
// seconds after segment one.
const vttSegmentLocal = `WEBVTT
X-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000

00:00:01.000 --> 00:00:02.000
a

WEBVTT
X-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:1800000

00:00:01.000 --> 00:00:02.000
b
`

func TestVTTTimestampMapSegmentLocal(t *testing.T) {
	cues := parseVTT(vttSegmentLocal)
	if len(cues) != 2 || cues[0].Start != 1 || cues[0].End != 2 || cues[1].Start != 11 || cues[1].End != 12 {
		t.Fatalf("cues = %+v, want a at 1-2 and b at 11-12", cues)
	}
}

// Cue times written in presentation time while the video's clock starts at ten
// seconds. The map says LOCAL ten seconds is PTS ten seconds, so after the
// video is moved to zero the cue has to move with it.
func TestVTTTimestampMapPresentationTime(t *testing.T) {
	vtt := "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:10.000\n\n" +
		"00:00:11.000 --> 00:00:12.000\nlate by ten without the map\n\n" +
		"00:00:09.500 --> 00:00:10.500\nbegan before the anchor\n"
	cues := parseVTT(vtt)
	if len(cues) != 2 || cues[1].Start != 1 || cues[1].End != 2 {
		t.Fatalf("cues = %+v, want the second cue at 1-2", cues)
	}
	if cues[0].Start != 0 || cues[0].End != 0.5 {
		t.Fatalf("early cue = %+v, want it clamped to 0-0.5", cues[0])
	}
}

func TestVTTWithoutTimestampMapUnchanged(t *testing.T) {
	cues := parseVTT(vttConcat)
	if cues[0].Start != 1 || cues[2].End != 7 {
		t.Fatalf("cues moved without a map: %+v", cues)
	}
}
