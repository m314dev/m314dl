// Package curl turns a copy-as-curl command line into a URL and a set of
// headers. A user can paste the browser's "Copy as cURL" output instead of
// typing every -H flag by hand.
package curl

import (
	"fmt"
	"net/http"
	"strings"
)

// Parse pulls the request URL and the headers out of a curl command. The URL
// comes from `--url X`, `--url=X`, or the first bare http(s) argument. Every
// `-H`/`--header` becomes a header. `-A`/`--user-agent`, `-e`/`--referer` and
// `-b`/`--cookie` are turned into the matching headers (User-Agent, Referer,
// Cookie). Other flags are ignored. For flags that take a value, the value is
// skipped as well, so it is never mistaken for the URL. Header keys are put in
// canonical form ("user-agent" becomes "User-Agent"), so a later -H given on
// our own command line replaces the same header cleanly.
func Parse(cmd string) (rawURL string, headers map[string]string, err error) {
	toks, err := tokenize(cmd)
	if err != nil {
		return "", nil, err
	}
	headers = map[string]string{}
	set := func(k, v string) {
		if v != "" {
			headers[http.CanonicalHeaderKey(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	addHeader := func(h string) {
		if k, v, ok := strings.Cut(h, ":"); ok {
			set(k, v)
		}
	}
	// next returns the token after the current one (a flag's value) and moves
	// i past it.
	i := 0
	next := func() string {
		if i+1 < len(toks) {
			i++
			return toks[i]
		}
		return ""
	}
	// valueFlags are flags that take a value. We skip that value so it is not
	// read as the URL.
	valueFlags := map[string]bool{
		"-X": true, "--request": true, "-d": true, "--data": true,
		"--data-raw": true, "--data-binary": true, "--data-ascii": true,
		"--data-urlencode": true, "-x": true, "--proxy": true, "-o": true,
		"--output": true, "--connect-timeout": true, "-m": true, "--max-time": true,
		"-w": true, "--write-out": true, "--retry": true, "-T": true, "--upload-file": true,
	}
	for ; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t == "curl":
		case t == "--url":
			rawURL = next()
		case strings.HasPrefix(t, "--url="):
			rawURL = strings.TrimPrefix(t, "--url=")
		case t == "-H" || t == "--header":
			addHeader(next())
		case strings.HasPrefix(t, "--header="):
			addHeader(strings.TrimPrefix(t, "--header="))
		case t == "-A" || t == "--user-agent":
			set("User-Agent", next())
		case strings.HasPrefix(t, "--user-agent="):
			set("User-Agent", strings.TrimPrefix(t, "--user-agent="))
		case t == "-e" || t == "--referer":
			ref, _, _ := strings.Cut(next(), ";") // curl allows "url;auto"; keep the url
			set("Referer", ref)
		case t == "-b" || t == "--cookie":
			if c := next(); strings.Contains(c, "=") { // "k=v" means a cookie string, not a file
				set("Cookie", c)
			}
		case valueFlags[t]:
			next() // skip the flag's value
		case strings.HasPrefix(t, "-"):
			// A flag we do not use (--compressed, -sSL, -k and so on). Skip it
			// and leave the tokens around it alone. Bundled short flags like
			// -sSL never carry the URL.
		default:
			if rawURL == "" && looksLikeURL(t) {
				rawURL = t
			}
		}
	}
	if rawURL == "" {
		return "", nil, fmt.Errorf("no http(s) URL found in curl command")
	}
	return rawURL, headers, nil
}

func looksLikeURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// tokenize splits a shell-style command line into words. It follows the shell
// rules: single quotes keep their contents as they are, inside double quotes a
// backslash escapes the next byte, a backslash outside quotes escapes the next
// byte, and a backslash right before a newline joins the two lines.
func tokenize(s string) ([]string, error) {
	var toks []string
	var cur strings.Builder
	inTok := false
	flush := func() {
		if inTok {
			toks = append(toks, cur.String())
			cur.Reset()
			inTok = false
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '\'':
			inTok = true
			j := i + 1
			for j < len(s) && s[j] != '\'' {
				cur.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated single quote")
			}
			i = j + 1
		case '"':
			inTok = true
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				cur.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated double quote")
			}
			i = j + 1
		case '\\':
			if i+1 < len(s) {
				if s[i+1] == '\n' { // backslash-newline: join the two lines
					i += 2
					continue
				}
				cur.WriteByte(s[i+1])
				inTok = true
				i += 2
			} else {
				i++
			}
		case ' ', '\t', '\n', '\r':
			flush()
			i++
		default:
			cur.WriteByte(c)
			inTok = true
			i++
		}
	}
	flush()
	return toks, nil
}
