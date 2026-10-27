// Package subs normalizes downloaded subtitle streams into clean SRT/VTT.
// Parsing is lenient on purpose: real-world TTML/VTT often breaks the rules,
// and a subtitle glitch must never fail a finished download.
package subs

import (
	"fmt"
	"html"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Cue struct {
	Start float64 // seconds
	End   float64
	Text  string
}

type Kind int

const (
	KindVTT Kind = iota
	KindTTML
	KindSRT
	KindFMP4 // wvtt/stpp inside fMP4; extracted natively
	KindUnknown
)

// Sniff detects the subtitle payload format.
func Sniff(b []byte) Kind {
	head := b[:min(1024, len(b))]
	s := strings.TrimLeft(string(head), "\uFEFF \t\r\n")
	switch {
	case strings.HasPrefix(s, "WEBVTT"):
		return KindVTT
	case strings.HasPrefix(s, "<?xml"), strings.HasPrefix(s, "<tt"), strings.Contains(s, "<tt:tt"):
		return KindTTML
	case regexp.MustCompile(`^\d+\s*\r?\n\d\d:\d\d:\d\d,\d\d\d`).MatchString(s):
		return KindSRT
	}
	for i := 4; i < len(head)-4; i++ {
		switch string(head[i : i+4]) {
		case "ftyp", "styp", "moof":
			return KindFMP4
		}
	}
	return KindUnknown
}

// Parse extracts cues from concatenated VTT or TTML segment payloads.
func Parse(b []byte, kind Kind) ([]Cue, error) {
	switch kind {
	case KindVTT, KindSRT:
		return parseVTT(string(b)), nil
	case KindTTML:
		return parseTTML(string(b)), nil
	}
	return nil, fmt.Errorf("subs: unsupported format")
}

var vttTimeRe = regexp.MustCompile(`(?:(\d+):)?(\d{1,2}):(\d{2})[.,](\d{1,3})`)
var vttCueLineRe = regexp.MustCompile(`^\s*(?:(?:(\d+):)?(\d{1,2}):(\d{2})[.,](\d{1,3}))\s*-->\s*(?:(?:(\d+):)?(\d{1,2}):(\d{2})[.,](\d{1,3}))`)

func parseTime(h, m, s, ms string) float64 {
	hh, _ := strconv.ParseFloat(h, 64)
	mm, _ := strconv.ParseFloat(m, 64)
	ss, _ := strconv.ParseFloat(s, 64)
	for len(ms) < 3 {
		ms += "0"
	}
	msf, _ := strconv.ParseFloat(ms, 64)
	return hh*3600 + mm*60 + ss + msf/1000
}

// parseVTT handles plain VTT/SRT and VTT segments joined end to end (repeated
// WEBVTT headers are skipped; duplicate cues from overlapping segments are
// removed later).
func parseVTT(text string) []Cue {
	var cues []Cue
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		m := vttCueLineRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		start := parseTime(m[1], m[2], m[3], m[4])
		end := parseTime(m[5], m[6], m[7], m[8])
		var body []string
		for j := i + 1; j < len(lines); j++ {
			l := strings.TrimRight(lines[j], " \t")
			if l == "" || strings.HasPrefix(l, "WEBVTT") || vttCueLineRe.MatchString(l) {
				break
			}
			body = append(body, l)
			i = j
		}
		// Strip WebVTT cue tags (<c.white>, <v Speaker>, inline <00:00.000>
		// timestamps and so on) and decode entities (&rlm;, &amp; and so on).
		// SRT has neither, so if they are left in they show up as literal
		// garbage in the player. The TTML path does the same normalisation.
		txt := strings.TrimSpace(strings.Join(body, "\n"))
		txt = html.UnescapeString(tagRe.ReplaceAllString(txt, ""))
		if txt = strings.TrimSpace(txt); txt != "" && end > start {
			cues = append(cues, Cue{start, end, txt})
		}
	}
	return dedupe(cues)
}

var ttmlPRe = regexp.MustCompile(`(?s)<p[^>]*?begin="([^"]+)"[^>]*?end="([^"]+)"[^>]*>(.*?)</p>`)
var ttmlPRe2 = regexp.MustCompile(`(?s)<p[^>]*?end="([^"]+)"[^>]*?begin="([^"]+)"[^>]*>(.*?)</p>`)
var tagRe = regexp.MustCompile(`<[^>]*>`)
var brRe = regexp.MustCompile(`<br\s*/?>`)
var tickRateRe = regexp.MustCompile(`tickRate="(\d+)"`)

// parseTTML pulls <p begin end> cues out with regular expressions. That
// survives the mismatched tags and namespace chaos that kill strict XML parsers
// on real streams.
func parseTTML(text string) []Cue {
	tickRate := 0.0
	if m := tickRateRe.FindStringSubmatch(text); m != nil {
		tickRate, _ = strconv.ParseFloat(m[1], 64)
	}
	parse := func(v string) float64 { return parseTTMLTime(v, tickRate) }
	var cues []Cue
	add := func(begin, end, body string) {
		s, e := parse(begin), parse(end)
		txt := brRe.ReplaceAllString(body, "\n")
		txt = tagRe.ReplaceAllString(txt, "")
		txt = strings.TrimSpace(html.UnescapeString(txt))
		if txt != "" && e > s {
			cues = append(cues, Cue{s, e, txt})
		}
	}
	for _, m := range ttmlPRe.FindAllStringSubmatch(text, -1) {
		add(m[1], m[2], m[3])
	}
	if len(cues) == 0 {
		for _, m := range ttmlPRe2.FindAllStringSubmatch(text, -1) {
			add(m[2], m[1], m[3])
		}
	}
	return dedupe(cues)
}

func parseTTMLTime(v string, tickRate float64) float64 {
	v = strings.TrimSpace(v)
	switch {
	case strings.HasSuffix(v, "t"):
		t, _ := strconv.ParseFloat(strings.TrimSuffix(v, "t"), 64)
		if tickRate > 0 {
			return t / tickRate
		}
		return t / 10000000 // common default 10MHz
	case strings.HasSuffix(v, "ms"):
		t, _ := strconv.ParseFloat(strings.TrimSuffix(v, "ms"), 64)
		return t / 1000
	case strings.HasSuffix(v, "s") && !strings.Contains(v, ":"):
		t, _ := strconv.ParseFloat(strings.TrimSuffix(v, "s"), 64)
		return t
	}
	if m := vttTimeRe.FindStringSubmatch(v); m != nil {
		return parseTime(m[1], m[2], m[3], m[4])
	}
	return 0
}

// dedupe sorts cues by time and merges exact duplicates, as well as identical
// cues that sit next to each other because live segments overlap.
func dedupe(cues []Cue) []Cue {
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].Start < cues[j].Start })
	var out []Cue
	for _, c := range cues {
		if n := len(out); n > 0 {
			prev := &out[n-1]
			if prev.Text == c.Text && c.Start <= prev.End+0.1 {
				if c.End > prev.End {
					prev.End = c.End
				}
				continue
			}
			if prev.Start == c.Start && prev.End == c.End && prev.Text == c.Text {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// Rebase shifts all cues so the first one starts at zero. This fixes live
// streams whose cue times are wall-clock times.
func Rebase(cues []Cue) []Cue {
	if len(cues) == 0 {
		return cues
	}
	off := cues[0].Start
	if off < 3600 { // only shift absurd offsets (wall-clock or PTS epochs)
		return cues
	}
	for i := range cues {
		cues[i].Start -= off
		cues[i].End -= off
	}
	return cues
}

func fmtSRTTime(t float64) string {
	ms := int(t*1000 + 0.5)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

func fmtVTTTime(t float64) string {
	ms := int(t*1000 + 0.5)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// WriteSRT and WriteVTT write the cues to a file in that format.
func WriteSRT(path string, cues []Cue) error {
	var b strings.Builder
	for i, c := range cues {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, fmtSRTTime(c.Start), fmtSRTTime(c.End), c.Text)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func WriteVTT(path string, cues []Cue) error {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, c := range cues {
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n", fmtVTTTime(c.Start), fmtVTTTime(c.End), c.Text)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
