package dash

import (
	"fmt"
	"strings"
	"testing"
)

// bigMPD builds a multi-period, multi-representation on-demand MPD with n
// SegmentTimeline entries per representation — a realistic large manifest.
func bigMPD(periods, reps, segs int) []byte {
	var b strings.Builder
	b.WriteString(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:cenc="urn:mpeg:cenc:2013" type="static" mediaPresentationDuration="PT2H">`)
	for p := 0; p < periods; p++ {
		fmt.Fprintf(&b, `<Period id="%d" start="PT%dS" duration="PT600S"><AdaptationSet contentType="video" mimeType="video/mp4">`, p, p*600)
		b.WriteString(`<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc" cenc:default_KID="d74e3f99-fcbf-4508-87ae-975e23e2c656"/>`)
		for r := 0; r < reps; r++ {
			fmt.Fprintf(&b, `<Representation id="v%d" bandwidth="%d" codecs="avc1.64001f" width="1920" height="1080"><SegmentTemplate timescale="1000" initialization="v%d/init.mp4" media="v%d/$Number$.m4s" startNumber="1"><SegmentTimeline>`, r, 1000000*(r+1), r, r)
			t := 0
			for s := 0; s < segs; s++ {
				fmt.Fprintf(&b, `<S t="%d" d="6000"/>`, t)
				t += 6000
			}
			b.WriteString(`</SegmentTimeline></SegmentTemplate></Representation>`)
		}
		b.WriteString(`</AdaptationSet></Period>`)
	}
	b.WriteString(`</MPD>`)
	return []byte(b.String())
}

// BenchmarkParse measures MPD parse throughput on a large manifest (4 periods ×
// 6 renditions × 100 segments = 2400 <S> entries).
func BenchmarkParse(b *testing.B) {
	mpd := bigMPD(4, 6, 100)
	b.SetBytes(int64(len(mpd)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Parse(mpd, "https://ex.com/m.mpd"); err != nil {
			b.Fatal(err)
		}
	}
}
