// Package manifest defines the streaming model shared by every protocol.
// The HLS, DASH and Smooth Streaming parsers all turn their input into these
// types, and the download engine only ever sees this model.
package manifest

import (
	"fmt"
	"time"
)

type MediaType int

const (
	Video MediaType = iota
	Audio
	Subtitles
	Unknown
)

func (t MediaType) String() string {
	switch t {
	case Video:
		return "video"
	case Audio:
		return "audio"
	case Subtitles:
		return "subtitles"
	}
	return "unknown"
}

// EncMethod says how a segment is encrypted.
type EncMethod string

const (
	EncNone      EncMethod = ""
	EncAES128    EncMethod = "AES-128"    // whole segment AES-CBC; decrypted in-process
	EncSampleAES EncMethod = "SAMPLE-AES" // unsupported: reported, never downloaded blindly
	EncCENC      EncMethod = "CENC"       // DRM: decrypted in process with a -key, refused without one
)

// ByteRange is an HTTP byte range; both ends are inclusive.
type ByteRange struct {
	Start int64
	End   int64 // -1 means no end: the range runs to the end of the object
}

func (r *ByteRange) Header() string {
	if r.End < 0 {
		return fmt.Sprintf("bytes=%d-", r.Start)
	}
	return fmt.Sprintf("bytes=%d-%d", r.Start, r.End)
}

type Key struct {
	Method EncMethod
	URI    string   // http(s) URL, data: URL, or empty
	IV     []byte   // nil: derive it from the segment's media sequence (HLS rule)
	KID    [16]byte // CENC default key id (manifest or init tenc box); all zero = unknown
}

type InitMap struct {
	URL   string
	Range *ByteRange
}

type Segment struct {
	URL           string
	Duration      float64 // seconds
	Range         *ByteRange
	Key           *Key // nil means the segment is not encrypted
	Init          *InitMap
	Seq           int64 // media sequence or $Number$; IV fallback and ordering
	Discontinuity bool
	// Index, when set, marks a placeholder: the segment is a whole on-demand
	// media file, and its SegmentIndexBox (sidx, the table of where each piece
	// of the file starts) sits at this byte range. source.ExpandIndexed splits
	// it into real subsegments before download.
	Index *ByteRange
}

// Stream is one downloadable track (video variant, audio rendition, subtitle).
type Stream struct {
	Type         MediaType
	ID           string // stable id used for selection and display
	PlaylistURL  string // media playlist or representation URL (for live refresh)
	Bandwidth    int64
	Width        int
	Height       int
	FrameRate    float64
	Codecs       string
	Language     string
	Name         string
	Channels     string
	Default      bool
	AudioGroup   string // HLS: the rendition groups this video variant references
	SubsGroup    string
	GroupID      string // HLS: the group this rendition belongs to
	MuxedAudio   bool   // HLS variant whose video segments also carry the audio
	Live         bool
	MediaSeq     int64 // first segment's media sequence (tracks the live window)
	TargetDur    float64
	Refresh      time.Duration // how often to re-fetch a live playlist
	Init         *InitMap
	Segments     []Segment
	SegmentsFull bool // segments already filled in (bare media playlist)

	// DASH: the AudioChannelConfiguration scheme; Channels holds its value.
	ChannelScheme string
}

// Resolution returns the picture size as "WxH", or "" when it is unknown.
func (s *Stream) Resolution() string {
	if s.Width == 0 && s.Height == 0 {
		return ""
	}
	return fmt.Sprintf("%dx%d", s.Width, s.Height)
}

// Duration returns the total length of all segments, in seconds.
func (s *Stream) Duration() float64 {
	var d float64
	for i := range s.Segments {
		d += s.Segments[i].Duration
	}
	return d
}

func (s *Stream) String() string {
	out := s.Type.String()
	if r := s.Resolution(); r != "" {
		out += " " + r
	}
	if s.Bandwidth > 0 {
		out += fmt.Sprintf(" %dkbps", s.Bandwidth/1000)
	}
	if s.Codecs != "" {
		out += " " + s.Codecs
	}
	if s.Language != "" {
		out += " [" + s.Language + "]"
	}
	if s.Name != "" {
		out += " (" + s.Name + ")"
	}
	if s.Live {
		out += " LIVE"
	}
	return out
}

// Master is the parsed top-level manifest.
type Master struct {
	URL     string // manifest URL after redirects; relative URLs resolve against it
	Live    bool
	Streams []*Stream
}
