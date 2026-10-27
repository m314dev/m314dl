package source

import (
	"context"
	"fmt"

	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
	"github.com/m314dev/m314dl/internal/mp4"
)

// ExpandIndexed replaces every SegmentBase placeholder in st (a whole
// on-demand media file with Segment.Index set) with the subsegments its sidx
// describes. The sidx is the file's index box: a table of where each piece
// starts and how long it plays. Each reference becomes one byte-range segment
// with its exact duration. The engine then fetches, decrypts and publishes a
// few seconds at a time instead of reading a whole film into RAM (a 4K feature
// was 11 GB, the OOM kill in issue #8). Only the selected streams are expanded,
// one small range request each. A placeholder whose index cannot be fetched or
// parsed is kept whole, with a warning, so a clear small file still downloads
// the old way.
//
// Several periods may slice the same file (each period has its own
// presentationTimeOffset into one BaseURL). The file is indexed once and later
// placeholders for it are dropped, since the first expansion already covers it
// end to end.
func ExpandIndexed(ctx context.Context, client *httpx.Client, st *manifest.Stream, logv func(string, ...any)) error {
	// Nothing to do unless some segment is a SegmentBase placeholder. Streams
	// built from a template or a list (every live stream, and template VOD) keep
	// their segments and their Seq untouched.
	hasIndex := false
	for i := range st.Segments {
		if st.Segments[i].Index != nil {
			hasIndex = true
			break
		}
	}
	if !hasIndex {
		return nil
	}
	var out []manifest.Segment
	seen := map[string]bool{}
	seq := int64(0)
	for i := range st.Segments {
		seg := st.Segments[i]
		if seg.Index == nil {
			seg.Seq = seq
			seq++
			out = append(out, seg)
			continue
		}
		key := seg.URL + "@" + seg.Index.Header()
		if seen[key] {
			continue
		}
		seen[key] = true
		subs, err := indexedSegments(ctx, client, seg)
		if err != nil {
			logv("dash: %s: %v; downloading the file as one segment", seg.URL, err)
			seg.Index = nil
			seg.Seq = seq
			seq++
			out = append(out, seg)
			continue
		}
		for j := range subs {
			subs[j].Seq = seq
			seq++
		}
		out = append(out, subs...)
	}
	st.Segments = out
	return nil
}

func indexedSegments(ctx context.Context, client *httpx.Client, seg manifest.Segment) ([]manifest.Segment, error) {
	body, _, err := client.FetchBytes(ctx, seg.URL, seg.Index.Header())
	if err != nil {
		return nil, fmt.Errorf("fetch sidx: %w", err)
	}
	idx, err := mp4.ParseSidx(body)
	if err != nil {
		return nil, err
	}
	if len(idx.Refs) == 0 {
		return nil, fmt.Errorf("sidx lists no subsegments")
	}
	pos := seg.Index.Start + idx.End + int64(idx.FirstOffset)
	subs := make([]manifest.Segment, 0, len(idx.Refs))
	for i, r := range idx.Refs {
		s := manifest.Segment{
			URL:      seg.URL,
			Duration: float64(r.Duration) / float64(idx.Timescale),
			Range:    &manifest.ByteRange{Start: pos, End: pos + int64(r.Size) - 1},
			Key:      seg.Key,
			Init:     seg.Init,
		}
		if i == 0 {
			s.Discontinuity = seg.Discontinuity
		}
		subs = append(subs, s)
		pos += int64(r.Size)
	}
	return subs, nil
}
