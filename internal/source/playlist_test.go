package source

import "testing"

func TestParsePlaylist(t *testing.T) {
	body := []byte(`#EXTM3U url-tvg="http://epg"
#EXTINF:-1 tvg-id="" tvg-name="Batman, Returns" tvg-logo="http://l/x.png" group-title="Movies | EN",Batman, Returns (1992)
http://h:80/movie/u/p/1.mkv
#EXTINF:-1 tvg-name="Only Name" group-title="",
#EXTGRP:Shows
series/u/p/2.mp4
http://h:80/u/p/3
`)
	if !isPlaylist(body) {
		t.Fatal("IPTV list not detected")
	}
	for _, hlsBody := range []string{
		"#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nseg1.ts\n",
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n",
	} {
		if isPlaylist([]byte(hlsBody)) {
			t.Fatalf("HLS taken for an IPTV list: %q", hlsBody)
		}
	}
	m := parsePlaylist(body, "http://h:80/get.php?type=m3u")
	want := []struct{ id, name, group, url string }{
		{"1", "Batman, Returns (1992)", "Movies | EN", "http://h:80/movie/u/p/1.mkv"},
		{"2", "Only Name", "Shows", "http://h:80/series/u/p/2.mp4"},
		{"3", "3", "", "http://h:80/u/p/3"},
	}
	if len(m.Streams) != len(want) {
		t.Fatalf("got %d entries, want %d", len(m.Streams), len(want))
	}
	for i, w := range want {
		e := m.Streams[i]
		if e.ID != w.id || e.Name != w.name || e.GroupID != w.group || e.PlaylistURL != w.url {
			t.Errorf("entry %d = {%s %q %q %s}, want %v", i, e.ID, e.Name, e.GroupID, e.PlaylistURL, w)
		}
	}
}

func TestLiveEntry(t *testing.T) {
	for u, live := range map[string]bool{
		"http://h/movie/u/p/1.mkv":  false,
		"http://h/series/u/p/2.mp4": false,
		"http://h/files/film.mp4":   false,
		"http://h/vod/show.m3u8":    false, // the manifest decides
		"http://h/live/u/p/3.m3u8":  true,
		"http://h/live/u/p/3.ts":    true,
		"http://h/u/p/3":            true, // Xtream short channel form
		"http://h/u/p/3.ts":         true,
	} {
		if got := LiveEntry(u); got != live {
			t.Errorf("LiveEntry(%s) = %v, want %v", u, got, live)
		}
	}
}
