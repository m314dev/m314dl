package source

import (
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/m314dev/m314dl/internal/manifest"
)

// An IPTV-style M3U playlist (#EXTM3U + #EXTINF lines, each followed by the
// URL of a whole movie, episode, or channel) is a list of downloads, not an
// HLS playlist: its URLs are separate titles, not segments of one. It comes
// back from LoadManifest as a master of kind PlaylistKind with one Stream per
// entry: ID is the entry's 1-based position in the file, Name its title,
// GroupID its group-title, PlaylistURL its URL.
const PlaylistKind = "playlist"

// isPlaylist tells an IPTV list from HLS. Every HLS media playlist carries
// #EXT-X-TARGETDURATION and every master #EXT-X-STREAM-INF (or #EXT-X-MEDIA);
// a list of titles has none of them.
func isPlaylist(body []byte) bool {
	s := string(body)
	return !strings.Contains(s, "#EXT-X-TARGETDURATION") &&
		!strings.Contains(s, "#EXT-X-STREAM-INF") &&
		!strings.Contains(s, "#EXT-X-MEDIA:")
}

// parsePlaylist reads the entries. The title is the #EXTINF text after the
// first comma outside quotes (falling back to tvg-name, then the URL's
// basename); the group is group-title, or a preceding #EXTGRP line.
func parsePlaylist(body []byte, baseURL string) *manifest.Master {
	m := &manifest.Master{URL: baseURL}
	base, _ := url.Parse(baseURL)
	var title, group string
	n := 0
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			attrs, t := splitExtinf(line[len("#EXTINF:"):])
			title, group = t, attrValue(attrs, "group-title")
			if title == "" {
				title = attrValue(attrs, "tvg-name")
			}
		case strings.HasPrefix(line, "#EXTGRP:"):
			if group == "" {
				group = strings.TrimSpace(line[len("#EXTGRP:"):])
			}
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			u := line
			if base != nil {
				if r, err := base.Parse(line); err == nil {
					u = r.String()
				}
			}
			if title == "" {
				if p, err := url.Parse(u); err == nil {
					title = strings.TrimSuffix(path.Base(p.Path), path.Ext(p.Path))
				}
			}
			n++
			m.Streams = append(m.Streams, &manifest.Stream{
				Type: manifest.Unknown, ID: strconv.Itoa(n), Name: title, GroupID: group, PlaylistURL: u,
			})
			title, group = "", ""
		}
	}
	return m
}

// splitExtinf splits `-1 key="v,al" key2="…",Title` at the first comma
// outside quotes.
func splitExtinf(s string) (attrs, title string) {
	inQuote := false
	for i, c := range s {
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			return s[:i], strings.TrimSpace(s[i+1:])
		}
	}
	return s, ""
}

func attrValue(attrs, key string) string {
	i := strings.Index(attrs, key+`="`)
	if i < 0 {
		return ""
	}
	v := attrs[i+len(key)+2:]
	if j := strings.IndexByte(v, '"'); j >= 0 {
		v = v[:j]
	}
	return strings.TrimSpace(v)
}

// vodExts are file containers an IPTV list uses for movies and episodes.
var vodExts = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".mov": true, ".m4v": true, ".webm": true,
	".wmv": true, ".flv": true, ".mpg": true, ".mpeg": true, ".3gp": true,
}

// LiveEntry guesses from the URL alone — no request, which matters on a panel
// that bans an IP for opening thousands of channels — whether a playlist entry
// is an endless live channel. Xtream Codes lists put movies and episodes under
// /movie/ and /series/ with a file extension, and channels under /live/ or a
// bare /user/pass/id (or id.ts). An .m3u8/.mpd entry is left to the manifest
// (HLS without ENDLIST is live) and so reports false here.
// ponytail: URL heuristic; a list serving VOD as bare or .ts URLs is skipped as
// live — probe Content-Length per entry if such lists show up.
func LiveEntry(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	p := strings.ToLower(u.Path)
	switch {
	case strings.Contains(p, "/live/"):
		return true
	case strings.Contains(p, "/movie/") || strings.Contains(p, "/series/"):
		return false
	}
	ext := path.Ext(p)
	return !vodExts[ext] && ext != ".m3u8" && ext != ".mpd"
}
