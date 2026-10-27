// Package source turns an input (a URL, a web page, or a local file) into a
// manifest. It also holds the small helpers that every mode (download,
// restream, worker) needs for the streams that come out of it.
package source

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/m314dev/m314dl/internal/dash"
	"github.com/m314dev/m314dl/internal/engine"
	"github.com/m314dev/m314dl/internal/hls"
	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
	"github.com/m314dev/m314dl/internal/mss"
	"github.com/m314dev/m314dl/internal/scrape"
)

// localManifestPath returns the file path and true when input is a local
// manifest: a file:// URL, or a string with no URL scheme that names an
// existing file.
func localManifestPath(input string) (string, bool) {
	if strings.HasPrefix(input, "file://") {
		return strings.TrimPrefix(input, "file://"), true
	}
	if u, err := url.Parse(input); err == nil && u.Scheme != "" {
		return "", false // has a real URL scheme such as http or https
	}
	if _, err := os.Stat(input); err == nil {
		return input, true
	}
	return "", false
}

// localBaseURL is the base URL that relative segment URLs in a local manifest
// resolve against. Absolute segment URLs (the common case for signed
// manifests) do not use it.
func localBaseURL(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return "file://" + filepath.Dir(abs) + "/"
}

// ParseKeys parses -key values into a map from KID to key. Each value is
// "KID:KEY" (both hex; dashes in the KID are optional) or a bare "KEY", which
// is stored under the all-zero KID and used when exactly one key is given and
// the KID does not match.
func ParseKeys(vals []string) (map[[16]byte][]byte, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	out := map[[16]byte][]byte{}
	for _, v := range vals {
		kidStr, keyStr, hasKID := strings.Cut(v, ":")
		if !hasKID {
			kidStr, keyStr = "", v
		}
		key, err := hex.DecodeString(strings.TrimSpace(keyStr))
		if err != nil || len(key) != 16 {
			return nil, fmt.Errorf("bad -key %q: key must be 32 hex chars (16 bytes)", v)
		}
		var kid [16]byte
		if hasKID {
			k, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(kidStr), "-", ""))
			if err != nil || len(k) != 16 {
				return nil, fmt.Errorf("bad -key %q: KID must be 32 hex chars (16 bytes)", v)
			}
			copy(kid[:], k)
		}
		out[kid] = key
	}
	return out, nil
}

// LoadManifest fetches the input, checks whether it is HLS, DASH or MSS, and
// falls back to scraping when the input is a web page. A direct file (anything
// binary, or anything served as an attachment) comes back as a one-stream
// master of kind FileKind whose segments are the file's byte ranges; see
// file.go. An IPTV M3U list comes back as kind PlaylistKind; see playlist.go.
func LoadManifest(ctx context.Context, client *httpx.Client, inputURL string, logv func(string, ...any)) (*manifest.Master, string, error) {
	// Local manifest file (a path or file://): some providers sign the playlist
	// per request and hand it over as text instead of a URL.
	if path, ok := localManifestPath(inputURL); ok {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("read manifest %s: %w", path, err)
		}
		base := localBaseURL(path)
		logv("local manifest %s (base %s)", path, base)
		if m, kind, err, ok := parseManifest(body, base); ok {
			return m, kind, err
		}
		return nil, "", fmt.Errorf("local file %s is not an HLS/DASH/MSS manifest", path)
	}
	// Open the response instead of fetching the whole body: the headers and the
	// first bytes say whether this is a file, and a multi-gigabyte file must not
	// be read into memory just to find that out.
	resp, err := client.Open(ctx, inputURL, "")
	if err != nil {
		return nil, "", fmt.Errorf("fetch %s: %w", inputURL, err)
	}
	defer resp.Body.Close()
	finalURL := httpx.FinalURL(resp, inputURL)
	head, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err == nil && isDirectFile(resp, head) {
		logv("input is a direct file (%s)", resp.Header.Get("Content-Type"))
		return fileMaster(ctx, client, resp, inputURL, finalURL, logv), FileKind, nil
	}
	var rest []byte
	if err == nil {
		rest, err = io.ReadAll(resp.Body)
	}
	body := append(head, rest...)
	if err != nil {
		// the read failed partway through the body: FetchBytes retries it whole
		if body, finalURL, err = client.FetchBytes(ctx, inputURL, ""); err != nil {
			return nil, "", fmt.Errorf("fetch %s: %w", inputURL, err)
		}
	}
	if m, kind, err, ok := parseManifest(body, finalURL); ok {
		return m, kind, err
	}
	// probably a web page: scrape it
	logv("input is not a manifest; scraping page for stream URLs")
	candidates, err := scrape.Find(ctx, client, inputURL)
	if err != nil {
		return nil, "", err
	}
	if len(candidates) == 0 {
		return nil, "", fmt.Errorf("no HLS/DASH/MSS manifest found at %s (not a playlist, and page scan found no stream URLs)", inputURL)
	}
	for _, c := range candidates {
		fmt.Fprintln(os.Stderr, "found stream: "+c)
	}
	first := candidates[0]
	logv("using first candidate: %s", first)
	body, finalURL, err = client.FetchBytes(ctx, first, "")
	if err != nil {
		return nil, "", fmt.Errorf("fetch scraped %s: %w", first, err)
	}
	if m, kind, err, ok := parseManifest(body, finalURL); ok {
		return m, kind, err
	}
	return nil, "", fmt.Errorf("scraped URL %s is not a recognizable manifest", first)
}

// parseManifest detects the manifest format and parses it. ok is false when
// the body is none of the known formats (most likely a web page).
func parseManifest(body []byte, baseURL string) (m *manifest.Master, kind string, err error, ok bool) {
	switch {
	case hls.IsHLS(body) && isPlaylist(body):
		return parsePlaylist(body, baseURL), PlaylistKind, nil, true
	case hls.IsHLS(body):
		m, err = hls.ParseMaster(body, baseURL)
		return m, "hls", err, true
	case dash.IsDASH(body):
		m, err = dash.Parse(body, baseURL)
		return m, "dash", err, true
	case mss.IsMSS(body):
		m, err = mss.Parse(body, baseURL)
		return m, "mss", err, true
	}
	return nil, "", nil, false
}

// RefreshFunc returns a function that re-fetches a live stream's playlist so
// the engine sees new segments.
func RefreshFunc(client *httpx.Client, kind string, st *manifest.Stream, logv func(string, ...any)) engine.RefreshFunc {
	return func(ctx context.Context) (*manifest.Stream, error) {
		body, finalURL, err := client.FetchBytes(ctx, st.PlaylistURL, "")
		if err != nil {
			return nil, err
		}
		if kind == "hls" {
			fresh := *st
			fresh.Segments = nil
			if err := hls.ParseMedia(body, finalURL, &fresh); err != nil {
				return nil, err
			}
			return &fresh, nil
		}
		// DASH and Smooth: re-parse the whole manifest and find the track again
		// by its ID, which stays the same across refreshes.
		var m *manifest.Master
		if kind == "mss" {
			m, err = mss.Parse(body, finalURL)
		} else {
			m, err = dash.Parse(body, finalURL)
		}
		if err != nil {
			return nil, err
		}
		for _, cand := range m.Streams {
			if cand.ID == st.ID {
				return cand, nil
			}
		}
		logv("live: stream %s missing from refreshed manifest", st.ID)
		return nil, fmt.Errorf("stream %s no longer in manifest", st.ID)
	}
}

// RawExt returns the file extension for a stream's raw downloaded data.
func RawExt(st *manifest.Stream) string {
	if st.Type == manifest.Subtitles {
		return ".rawsub"
	}
	u := ""
	if len(st.Segments) > 0 {
		u = st.Segments[0].URL
	}
	if st.Init != nil || strings.Contains(u, ".m4s") || strings.Contains(u, ".mp4") {
		return ".mp4"
	}
	return ".ts"
}

var tagBad = regexp.MustCompile(`[^\w.\-]+`)

// SanitizeTag makes a language or name tag safe to use in filenames and URLs.
func SanitizeTag(s string) string { return tagBad.ReplaceAllString(s, "_") }
