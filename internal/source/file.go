package source

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/m314dev/m314dl/internal/dash"
	"github.com/m314dev/m314dl/internal/hls"
	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
	"github.com/m314dev/m314dl/internal/mss"
)

// A plain file (mp4, mkv, zip, iso, and so on) goes through the same pipeline
// as a stream: one "stream" whose segments are consecutive byte ranges of the
// file, fetched in parallel and joined back together in order.

const (
	// fileChunk is the size of one byte-range slice. At 4 MiB the per-request
	// cost (TCP ramp-up, headers, a part file, a checkpoint write) stays well
	// under 1% at CDN speeds, the ordered writer holds at most one slice in
	// memory, and a 256 MiB file already keeps all 64 adaptive workers busy.
	// A file that would need more than fileMaxChunks slices gets bigger ones
	// instead, so the slice count (and the "segs" figure in the progress line)
	// stays readable.
	fileChunk     = 4 << 20
	fileMaxChunks = 4096
	// fileMaxSniff is the most text LoadManifest will read while looking for a
	// manifest or scraping a page. A text body bigger than any playlist or web
	// page is treated as a file.
	fileMaxSniff = 32 << 20
	// FileKind is the kind LoadManifest returns for a direct file.
	FileKind = "file"
)

// planChunks splits size bytes into consecutive byte ranges (both ends
// inclusive) that together cover [0, size).
func planChunks(size int64) []manifest.ByteRange {
	c := int64(fileChunk)
	if n := (size + c - 1) / c; n > fileMaxChunks {
		c = (size + fileMaxChunks - 1) / fileMaxChunks
	}
	var out []manifest.ByteRange
	for start := int64(0); start < size; start += c {
		end := start + c - 1
		if end >= size {
			end = size - 1
		}
		out = append(out, manifest.ByteRange{Start: start, End: end})
	}
	return out
}

// isDirectFile looks at the response headers and the first bytes of the body
// and decides whether the input is a file to download as is, or a manifest or
// web page to scrape. A manifest is always text, so a binary body cannot be
// one. A text body stays on the manifest/scrape path (a JSON or plain text
// endpoint may well carry stream URLs) unless it is served as an attachment or
// is far too big to be a playlist or a page.
func isDirectFile(resp *http.Response, head []byte) bool {
	// A body that is itself a manifest is never a direct file, whatever the
	// headers claim. Some CDNs force a download of playlists with
	// Content-Disposition: attachment (Tencent COS sets it on every object),
	// which would otherwise save the raw .m3u8 or .mpd as the "download". The
	// bytes settle the question, so check them before trusting the header.
	if hls.IsHLS(head) || dash.IsDASH(head) || mss.IsMSS(head) {
		return false
	}
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Disposition")), "attachment") {
		return true
	}
	if isHTML(resp.Header.Get("Content-Type")) {
		return false
	}
	if resp.ContentLength > fileMaxSniff {
		return true
	}
	return !strings.HasPrefix(http.DetectContentType(head), "text/")
}

func isHTML(contentType string) bool {
	mt, _, _ := mime.ParseMediaType(contentType)
	return mt == "text/html" || mt == "application/xhtml+xml"
}

// fileName picks the output filename. It uses the Content-Disposition filename
// when there is one, else the last path element of the final URL. When a
// redirect landed on an opaque token path with no extension (IPTV panels,
// signed CDNs) and the given URL names the file, the given URL's name wins.
// That name is also the same on every run, which resume needs, since the token
// changes every time. The result is cut down to a bare basename so a hostile
// header cannot point outside the output directory.
func fileName(resp *http.Response, inputURL, finalURL string) string {
	name := ""
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		name = params["filename"]
	}
	if name == "" {
		name = urlBase(finalURL)
		if in := urlBase(inputURL); filepath.Ext(name) == "" && filepath.Ext(in) != "" {
			name = in
		}
	}
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == ".." || name == string(filepath.Separator) || name == "" {
		return "m314dl-output"
	}
	if len(name) > 200 {
		ext := filepath.Ext(name)
		name = name[:200-len(ext)] + ext
	}
	return name
}

func urlBase(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return path.Base(u.Path)
}

// fileMaster builds the one-stream master for a direct file. When the length
// is known and the server honors a byte-range probe, the stream is the file
// sliced into ranges. Otherwise (unknown or chunked length, ranges ignored, or
// a file no bigger than one slice) it is a single whole-file segment, which
// the engine streams on one connection and resumes by byte offset.
//
// PlaylistURL is the URL as given, before redirects: requesting it again
// yields a fresh redirect token when the one in finalURL expires during the
// download.
func fileMaster(ctx context.Context, client *httpx.Client, resp *http.Response, inputURL, finalURL string, logv func(string, ...any)) *manifest.Master {
	st := &manifest.Stream{Type: manifest.Unknown, ID: "file", Name: fileName(resp, inputURL, finalURL), PlaylistURL: inputURL}
	size := resp.ContentLength
	if size <= 0 {
		ct := resp.Header.Get("Content-Type")
		if ct == "video/mp2t" || ct == "video/MP2T" || ct == "application/octet-stream" {
			st.Live = true
			st.Segments = []manifest.Segment{{URL: finalURL}}
			logv("file: live MPEG-TS stream (no Content-Length)")
			return &manifest.Master{URL: finalURL, Live: true, Streams: []*manifest.Stream{st}}
		}
	}
	var chunks []manifest.ByteRange
	if size > fileChunk && rangesHonored(ctx, client, finalURL, size) {
		chunks = planChunks(size)
	}
	if len(chunks) < 2 {
		st.Segments = []manifest.Segment{{URL: finalURL}}
		logv("file: %s, %d bytes, single connection", st.Name, size)
	} else {
		for i := range chunks {
			st.Segments = append(st.Segments, manifest.Segment{URL: finalURL, Range: &chunks[i], Seq: int64(i)})
		}
		logv("file: %s, %d bytes in %d ranges of %d", st.Name, size, len(chunks), chunks[0].End+1)
	}
	return &manifest.Master{URL: finalURL, Streams: []*manifest.Stream{st}}
}

// rangesHonored asks for the file's first byte and checks that the server
// really slices: a 206 status whose Content-Range total matches the length.
// Trusting the Accept-Ranges header alone is not enough. Some servers
// advertise it and then answer a ranged request with the whole file, and
// others honor ranges without advertising them.
func rangesHonored(ctx context.Context, client *httpx.Client, u string, size int64) bool {
	resp, err := client.Open(ctx, u, "bytes=0-0")
	if err != nil {
		return false
	}
	resp.Body.Close()
	var start, end, total int64
	_, err = fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total)
	return resp.StatusCode == http.StatusPartialContent && err == nil && start == 0 && end == 0 && total == size
}

// FileSize returns a direct-file stream's length when its ranges make it
// known, and -1 otherwise (a single-connection stream).
func FileSize(st *manifest.Stream) int64 {
	if n := len(st.Segments); n > 0 && st.Segments[n-1].Range != nil {
		return st.Segments[n-1].Range.End + 1
	}
	return -1
}
