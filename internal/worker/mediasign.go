package worker

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Signed media URLs.
//
// A worker's media routes face the players, and the control API never does.
// With no secret the only guard on /{id}/... is that the id is hard to guess.
// With -worker-media-secret set, every media request must carry an expiry and
// a signature in its query, and the controller that hands a player its URL is
// the one that signs it:
//
//	sig = hex(HMAC-SHA256(secret, path + "\n" + exp))
//
// path is the request path as sent, for example /news/live.m3u8, and exp the
// expiry as decimal Unix seconds. A leaked link names one object and is worth
// nothing past exp.
//
// A player only ever gets the entry URL from the controller. Everything else,
// the variant playlists, the init and the segments, it reads out of the
// playlists this worker serves. So on a signed request the worker signs what
// it writes into the body: each URI gets the same exp and a signature over the
// channel's prefix, /{id}/, which a request for anything under the channel may
// also present. One signature per body keeps the rewrite to a string append,
// and the grant is still bounded by the channel and the expiry the controller
// chose.

func signPath(secret, path string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(path))
	mac.Write([]byte("\n"))
	mac.Write([]byte(strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// checkSigned verifies a media request for channel id. It returns the expiry
// it was signed with, or the status and reason to refuse it with.
func checkSigned(secret, id string, r *http.Request) (exp int64, status int, reason string) {
	q := r.URL.Query()
	rawExp, sig := q.Get("exp"), q.Get("sig")
	if rawExp == "" || sig == "" {
		return 0, http.StatusForbidden, "this media URL is not signed"
	}
	exp, err := strconv.ParseInt(rawExp, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return 0, http.StatusForbidden, "this media URL has expired"
	}
	for _, p := range []string{r.URL.Path, "/" + id + "/"} {
		if subtle.ConstantTimeCompare([]byte(signPath(secret, p, exp)), []byte(sig)) == 1 {
			return exp, 0, ""
		}
	}
	return 0, http.StatusForbidden, "bad signature"
}

// isPlaylistPath says whether a media path is a body with URIs in it that
// need signing on the way out.
func isPlaylistPath(p string) bool {
	return strings.HasSuffix(p, ".m3u8") || strings.HasSuffix(p, ".mpd")
}

// signingWriter holds a playlist back, appends the query to every URI in it
// and sends it on. Only playlists go through it; segments are passed straight
// to the viewer.
type signingWriter struct {
	http.ResponseWriter
	query  string // "exp=...&sig=..."
	status int
	buf    bytes.Buffer
}

func (s *signingWriter) WriteHeader(code int)        { s.status = code }
func (s *signingWriter) Write(p []byte) (int, error) { return s.buf.Write(p) }

var (
	m3u8URIAttr = regexp.MustCompile(`URI="([^"]*)"`)
	mpdURLAttr  = regexp.MustCompile(`(initialization|media)="([^"]*)"`)
)

// finish writes the held body out with its URIs signed. An error body (a 503
// while the presentation is not ready) has no URIs and goes out as it is.
func (s *signingWriter) finish(path string) {
	body := s.buf.Bytes()
	if s.status == 0 || s.status == http.StatusOK {
		if strings.HasSuffix(path, ".mpd") {
			body = mpdURLAttr.ReplaceAll(body, []byte(`$1="$2?`+strings.ReplaceAll(s.query, "&", "&amp;")+`"`))
		} else {
			body = s.signM3U8(body)
		}
	}
	h := s.Header()
	h.Del("Content-Length")
	if s.status != 0 {
		s.ResponseWriter.WriteHeader(s.status)
	}
	s.ResponseWriter.Write(body)
}

func (s *signingWriter) signM3U8(body []byte) []byte {
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		switch {
		case line == "":
		case strings.HasPrefix(line, "#"):
			lines[i] = m3u8URIAttr.ReplaceAllString(line, `URI="$1?`+s.query+`"`)
		default:
			lines[i] = line + "?" + s.query
		}
	}
	return []byte(strings.Join(lines, "\n"))
}
