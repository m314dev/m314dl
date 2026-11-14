package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/source"
)

// tsSource serves body as a raw transport stream with no length, as a live
// source does, and returns its URL.
func tsSource(t *testing.T, body []byte) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.(http.Flusher).Flush() // no Content-Length
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/live.ts"
}

// Without -serve, a live MPEG-TS source is recorded exactly like any direct
// file: the bytes as they came, with nothing left beside the output.
func TestDownloadLiveTSRecords(t *testing.T) {
	body := bytes.Repeat(append([]byte{0x47, 0x01, 0x00, 0x10}, make([]byte, 184)...), 2000)
	u := tsSource(t, body)
	client, _ := httpx.New(httpx.Options{Retries: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, kind, err := source.LoadManifest(ctx, client, u, func(string, ...any) {})
	if err != nil || kind != source.LiveTSKind {
		t.Fatalf("kind %q, err %v", kind, err)
	}
	out := filepath.Join(t.TempDir(), "rec.ts")
	j := job{o: &options{output: out}, client: client, logv: func(string, ...any) {}}
	if err := j.download(ctx, cancel, u, m, kind); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(out); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("recording: %d bytes, err %v; want the %d bytes sent", len(got), err, len(body))
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(out), "rec.*")); len(left) != 1 {
		t.Fatalf("left beside the output: %v", left)
	}
}

// -list lists a live MPEG-TS source like a direct file and exits, even with
// -serve, as it does for every other input.
func TestListLiveTSWithServe(t *testing.T) {
	u := tsSource(t, bytes.Repeat(append([]byte{0x47, 0x01, 0x00, 0x10}, make([]byte, 184)...), 100))
	client, _ := httpx.New(httpx.Options{Retries: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, kind, err := source.LoadManifest(ctx, client, u, func(string, ...any) {})
	if err != nil || kind != source.LiveTSKind {
		t.Fatalf("kind %q, err %v", kind, err)
	}
	j := job{o: &options{listOnly: true, serve: "127.0.0.1:0"}, client: client, logv: func(string, ...any) {}}
	done := make(chan error, 1)
	go func() { done <- j.download(ctx, cancel, u, m, kind) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("-list with -serve started a restream instead of listing")
	}
}

// An M3U list's raw live TS channel is skipped like any other live entry. The
// list run sets skipLive; recording it would never end.
func TestPlaylistSkipsLiveTS(t *testing.T) {
	u := tsSource(t, bytes.Repeat(append([]byte{0x47, 0x01, 0x00, 0x10}, make([]byte, 184)...), 100))
	client, _ := httpx.New(httpx.Options{Retries: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, kind, err := source.LoadManifest(ctx, client, u, func(string, ...any) {})
	if err != nil || kind != source.LiveTSKind {
		t.Fatalf("kind %q, err %v", kind, err)
	}
	j := job{o: &options{output: filepath.Join(t.TempDir(), "x.ts")}, client: client, logv: func(string, ...any) {}, skipLive: true}
	if err := j.download(ctx, cancel, u, m, kind); !errors.Is(err, errLiveEntry) {
		t.Fatalf("download = %v, want errLiveEntry", err)
	}
}
