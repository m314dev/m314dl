// Package scrape finds stream manifest URLs inside ordinary web pages.
package scrape

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"github.com/m314dev/m314dl/internal/httpx"
)

// absRe finds absolute manifest URLs in page source and inline JSON, where a
// slash may be written as \/.
var absRe = regexp.MustCompile(`https?:(?:\\?/){2}[^\s"'<>\\]+?(?:\.(?:m3u8|mpd)|\.isml?/[Mm]anifest)(?:\?[^\s"'<>\\]*)?`)

// attrRe finds relative manifest paths in attributes such as src, href, file
// and source.
var attrRe = regexp.MustCompile(`(?:src|href|file|source|data-src|content)\s*=\s*["']([^"']+?(?:\.(?:m3u8|mpd)|\.isml?/[Mm]anifest)(?:\?[^"']*)?)["']`)

var iframeRe = regexp.MustCompile(`<iframe[^>]+src\s*=\s*["']([^"']+)["']`)

// Find fetches pageURL and returns the manifest URLs it finds, in page order
// with duplicates removed. It follows iframes one level deep.
// ponytail: static HTML+JSON scan only; a URL that JavaScript builds at run
// time needs a headless browser. Add CDP sniffing if this misses too much.
func Find(ctx context.Context, client *httpx.Client, pageURL string) ([]string, error) {
	urls, err := findInPage(ctx, client, pageURL)
	if err != nil {
		return nil, err
	}
	if len(urls) > 0 {
		return urls, nil
	}
	// nothing on the page itself: look one level down, into its iframes
	body, finalURL, err := client.FetchBytes(ctx, pageURL, "")
	if err != nil {
		return nil, err
	}
	for _, m := range iframeRe.FindAllStringSubmatch(string(body), -1) {
		iframeURL := resolve(finalURL, m[1])
		if iframeURL == "" || strings.HasPrefix(iframeURL, "about:") {
			continue
		}
		if found, err := findInPage(ctx, client, iframeURL); err == nil && len(found) > 0 {
			urls = append(urls, found...)
		}
	}
	return dedupe(urls), nil
}

func findInPage(ctx context.Context, client *httpx.Client, pageURL string) ([]string, error) {
	body, finalURL, err := client.FetchBytes(ctx, pageURL, "")
	if err != nil {
		return nil, err
	}
	text := string(body)
	var out []string
	for _, m := range absRe.FindAllString(text, -1) {
		out = append(out, strings.ReplaceAll(m, `\/`, "/"))
	}
	for _, m := range attrRe.FindAllStringSubmatch(text, -1) {
		if u := resolve(finalURL, m[1]); u != "" {
			out = append(out, u)
		}
	}
	return dedupe(out), nil
}

func resolve(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ""
	}
	return b.ResolveReference(r).String()
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range in {
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}
