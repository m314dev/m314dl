package hls

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mohamed/m314dl/internal/manifest"
)

// BenchmarkParseMedia measures media-playlist parse throughput on a long VOD
// playlist (3000 segments with EXT-X-MAP and byte ranges).
func BenchmarkParseMedia(b *testing.B) {
	const n = 3000
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:6\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MAP:URI=\"init.mp4\"\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "#EXTINF:6.000,\nseg%d.m4s\n", i)
	}
	sb.WriteString("#EXT-X-ENDLIST\n")
	body := []byte(sb.String())
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st := &manifest.Stream{Type: manifest.Video}
		if err := ParseMedia(body, "https://ex.com/media.m3u8", st); err != nil {
			b.Fatal(err)
		}
	}
}
