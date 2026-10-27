package subs

import (
	"fmt"
	"html"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/m314dev/m314dl/internal/mp4"
)

// ParseWVTT reads WebVTT cues stored as MP4 samples (the wvtt format). Cue
// timing comes from the MP4 sample table; the payl text carries none of it.
// Empty vtte samples mark gaps and still move that timeline forward. As with
// plain VTT conversion, layout and styling are reduced to plain cue text.
func ParseWVTT(data []byte) ([]Cue, error) {
	var cues []Cue
	type cueKey struct{ text, id, settings string }
	last := map[cueKey]int{}
	err := mp4.WalkSamples(data, "wvtt", func(sample mp4.TimedSample) error {
		return mp4.WalkBoxes(sample.Data, func(typ string, payload []byte) error {
			if typ == "vtte" {
				return nil
			}
			if typ != "vttc" {
				return fmt.Errorf("wvtt: unexpected sample box %q", typ)
			}
			var key cueKey
			havePayload := false
			if err := mp4.WalkBoxes(payload, func(typ string, b []byte) error {
				switch typ {
				case "payl":
					if !utf8.Valid(b) {
						return fmt.Errorf("wvtt: invalid UTF-8 cue")
					}
					key.text = strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(string(b), "")))
					havePayload = true
				case "iden":
					key.id = string(b)
				case "sttg":
					key.settings = string(b)
					// ctim is skipped: it holds the cue's original time, which is
					// different from this sample's playback time.
				}
				return nil
			}); err != nil {
				return err
			}
			if !havePayload {
				return fmt.Errorf("wvtt: cue missing payl")
			}
			if key.text == "" {
				return nil
			}
			if i, ok := last[key]; ok && math.Abs(cues[i].End-sample.Start) < 1e-9 {
				cues[i].End = sample.End // the cue continues in the next sample; extend it
			} else {
				last[key] = len(cues)
				cues = append(cues, Cue{Start: sample.Start, End: sample.End, Text: key.text})
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].Start < cues[j].Start })
	return cues, nil
}
