package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/source"
)

// An IPTV list, from a URL: VOD titles land in <dir>/<group>/<title>.<ext>, the
// live channel is never requested, and a rerun skips what is already done.
// -titles 1 runs one title at a time; -titles 2 runs both at once.
func TestDownloadPlaylist(t *testing.T) {
	for _, titles := range []int{1, 2} {
		if got := playlistRun(t, titles); got != titles {
			t.Errorf("-titles %d: %d titles in flight at once, want %d", titles, got, titles)
		}
	}
}

// playlistRun downloads a two-title list twice and returns the most titles
// the server saw in flight at once.
func playlistRun(t *testing.T, titles int) int {
	movie, episode := bytes.Repeat([]byte{0, 1, 2}, 1000), bytes.Repeat([]byte{0, 3}, 1000) // binary: a direct file
	var mu sync.Mutex
	hits := map[string]int{}
	inFlight, maxInFlight := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		var body []byte
		switch r.URL.Path {
		case "/get.m3u":
			w.Write([]byte("#EXTM3U\n" +
				"#EXTINF:-1 group-title=\"Movies: EN\",Heat? (1995)\n/movie/u/p/1.mkv\n" +
				"#EXTINF:-1 group-title=\"Live\",News HD\n/live/u/p/9.ts\n" +
				"#EXTINF:-1 group-title=\"Shows\",Show S01E01\n/series/u/p/2.mp4\n"))
			return
		case "/movie/u/p/1.mkv":
			body = movie
		case "/series/u/p/2.mp4":
			body = episode
		default:
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		inFlight++
		maxInFlight = max(maxInFlight, inFlight)
		mu.Unlock()
		// Hold before answering: a title's next request waits on this answer,
		// so only requests from different titles can be held at once.
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()
	client, _ := httpx.New(httpx.Options{Retries: 1})
	dir := t.TempDir()
	j := job{o: &options{output: dir, titles: titles}, client: client, logv: func(string, ...any) {}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for run := 0; run < 2; run++ {
		pl, kind, err := source.LoadManifest(ctx, client, srv.URL+"/get.m3u", j.logv)
		if err != nil || kind != source.PlaylistKind {
			t.Fatalf("kind %q, err %v", kind, err)
		}
		if err := downloadPlaylist(ctx, cancel, j, pl); err != nil {
			t.Fatal(err)
		}
		for path, want := range map[string][]byte{
			filepath.Join(dir, "Movies_ EN", "Heat_ (1995).mkv"): movie,
			filepath.Join(dir, "Shows", "Show S01E01.mp4"):       episode,
		} {
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("run %d: %s: %d bytes, err %v", run, path, len(got), err)
			}
		}
	}
	if hits["/live/u/p/9.ts"] != 0 {
		t.Fatal("live channel was requested")
	}
	return maxInFlight
}

func TestSelectEntries(t *testing.T) {
	pl, _, err := source.LoadManifest(context.Background(), nil, writeList(t), func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	ids := func(pick, group, match string) string {
		es, err := selectEntries(pl.Streams, pick, group, match)
		if err != nil {
			t.Fatal(err)
		}
		var s []string
		for _, e := range es {
			s = append(s, e.ID)
		}
		return strings.Join(s, ",")
	}
	for _, c := range []struct{ pick, group, match, want string }{
		{"", "", "", "1,2,3,4"},
		{"2-3", "", "", "2,3"},
		{"1,3-4", "movies", "", "1,3"},
		{"", "", "BATMAN", "1,4"},
		{"", "^shows$", "batman", "4"},
	} {
		if got := ids(c.pick, c.group, c.match); got != c.want {
			t.Errorf("pick %q group %q match %q = %s, want %s", c.pick, c.group, c.match, got, c.want)
		}
	}
	for _, bad := range []string{"0", "3-1", "a", "1,,2"} {
		if _, err := parsePick(bad); err == nil {
			t.Errorf("-pick %q accepted", bad)
		}
	}
}

// writeList is a local M3U file, the other input form.
func writeList(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "list.m3u")
	os.WriteFile(p, []byte("#EXTM3U\n"+
		"#EXTINF:-1 group-title=\"Movies\",Batman\nhttp://h/movie/1.mkv\n"+
		"#EXTINF:-1 group-title=\"Movies\",Heat\nhttp://h/movie/2.mkv\n"+
		"#EXTINF:-1 group-title=\"Movies\",Alien\nhttp://h/movie/3.mkv\n"+
		"#EXTINF:-1 group-title=\"Shows\",Batman S01E01\nhttp://h/series/4.mkv\n"), 0o644)
	return p
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		`AC/DC: Live?`:        "AC_DC_ Live_",
		"  trailing dots... ": "trailing dots",
		"..":                  "def",
		"":                    "def",
		"فيلم عربي":           "فيلم عربي",
	} {
		if got := safeName(in, "def"); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := safeName(strings.Repeat("ع", 300), ""); len([]rune(got)) != 100 {
		t.Errorf("long name not capped at 100 characters: %d", len([]rune(got)))
	}
}
