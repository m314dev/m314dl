package source

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/m314dev/m314dl/internal/engine"
	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
)

// tsStream returns n TS packets numbered from first: sync byte, PID 0x100,
// payload only, and the packet's number in its first payload bytes, so a test
// can tell which packets came through and in what order.
func tsStream(first, n int) []byte {
	b := make([]byte, n*tsPacket)
	for i := 0; i < n; i++ {
		p := b[i*tsPacket:]
		p[0], p[1], p[2], p[3] = 0x47, 0x01, 0x00, 0x10|byte((first+i)&0x0f)
		binary.BigEndian.PutUint32(p[4:], uint32(first+i))
	}
	return b
}

// packetNumbers reads tsStream's numbers back, failing on a torn packet.
func packetNumbers(t *testing.T, b []byte) []int {
	t.Helper()
	if len(b)%tsPacket != 0 {
		t.Fatalf("%d bytes is not whole packets", len(b))
	}
	var out []int
	for off := 0; off < len(b); off += tsPacket {
		if b[off] != 0x47 {
			t.Fatalf("packet at %d has no sync byte", off)
		}
		out = append(out, int(binary.BigEndian.Uint32(b[off+4:])))
	}
	return out
}

func wantRun(t *testing.T, got []int, first, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("got %d packets, want %d", len(got), n)
	}
	for i, v := range got {
		if v != first+i {
			t.Fatalf("packet %d is number %d, want %d", i, v, first+i)
		}
	}
}

// endless serves TS packets until the client goes away, 100 at a time.
func endless(w http.ResponseWriter, r *http.Request) {
	w.(http.Flusher).Flush() // headers out with no Content-Length: chunked
	for i := 0; r.Context().Err() == nil; i += 100 {
		if _, err := w.Write(tsStream(i, 100)); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		time.Sleep(time.Millisecond)
	}
}

func TestFindSync(t *testing.T) {
	ts := tsStream(0, 10)
	cases := []struct {
		name string
		b    []byte
		want int
	}{
		{"aligned", ts, 0},
		{"junk before the first packet", append([]byte("junk!"), ts...), 5},
		{"one byte short of a run", ts[:tsProbePackets*tsPacket-1], -1},
		// The file tests' blob carries three sync bytes 188 apart on purpose.
		{"file with a few sync bytes", blob(4096), -1},
		{"text full of G", bytes.Repeat([]byte("GET / HTTP/1.1\r\n"), 200), -1},
	}
	for _, c := range cases {
		if got := findSync(c.b, tsProbePackets); got != c.want {
			t.Errorf("%s: findSync = %d, want %d", c.name, got, c.want)
		}
	}
}

// A raw TS body with no length is a live stream; with a length it is a
// recording; an ordinary file served chunked as application/octet-stream stays
// a file. The live master is the file master unchanged, so the download paths
// see no difference; only LiveTSTrack makes it a live video track.
func TestLoadManifestLiveTS(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/live.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		endless(w, r)
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.(http.Flusher).Flush()
		w.Write([]byte{0, 0, 0}) // a stray prefix before the first packet
		endless(w, r)
	})
	mux.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/live.ts?token=abc", http.StatusFound)
	})
	mux.HandleFunc("/recording.ts", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "recording.ts", time.Time{}, bytes.NewReader(tsStream(0, 100)))
	})
	mux.HandleFunc("/chunked", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.(http.Flusher).Flush()
		w.Write(blob(64 << 10))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client, _ := httpx.New(httpx.Options{Retries: 1})
	logv := func(string, ...any) {}

	for _, c := range []struct{ path, kind string }{
		{"/live.ts", LiveTSKind},
		{"/stream", LiveTSKind},
		{"/redir", LiveTSKind},
		{"/recording.ts", FileKind},
		{"/chunked", FileKind},
	} {
		m, kind, err := LoadManifest(context.Background(), client, srv.URL+c.path, logv)
		if err != nil || kind != c.kind {
			t.Errorf("%s: kind %q err %v, want %q", c.path, kind, err, c.kind)
			continue
		}
		if kind != LiveTSKind {
			continue
		}
		st := m.Streams[0]
		if len(m.Streams) != 1 || m.Live || st.Live || st.Type != manifest.Unknown || len(st.Segments) != 1 || st.Segments[0].Range != nil {
			t.Errorf("%s: master should be the plain file master: live=%v %+v", c.path, m.Live, st)
		}
		if st.PlaylistURL != srv.URL+c.path {
			t.Errorf("%s: PlaylistURL %q should be the URL as given", c.path, st.PlaylistURL)
		}
		tr := LiveTSTrack(m)
		if tr.Type != manifest.Video || !tr.Live || !tr.MuxedAudio || tr.PlaylistURL != st.PlaylistURL || st.Live {
			t.Errorf("%s: LiveTSTrack = %+v (source stream now %+v)", c.path, tr, st)
		}
	}
	if m, _, _ := LoadManifest(context.Background(), client, srv.URL+"/redir", logv); !strings.Contains(m.Streams[0].Segments[0].URL, "token=abc") {
		t.Errorf("redirect: the segment should carry the final URL, got %s", m.Streams[0].Segments[0].URL)
	}
}

// chunkSink records what PipeLiveTS hands over, and calls enough once it has
// at least want packets.
type chunkSink struct {
	mu     sync.Mutex
	data   []byte
	small  int // chunks under pipeChunk
	want   int
	enough func()
}

func (s *chunkSink) Init([]byte) error { return nil }

func (s *chunkSink) Segment(_ engine.SegmentInfo, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(b) == 0 || len(b)%tsPacket != 0 || b[0] != 0x47 {
		return errors.New("sink got a chunk that is not whole packets")
	}
	s.data = append(s.data, b...)
	if len(b) < pipeChunk {
		s.small++
	}
	if s.want > 0 && len(s.data)/tsPacket >= s.want && s.enough != nil {
		s.enough()
	}
	return nil
}

func (s *chunkSink) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data...)
}

// fastPipe shortens the pipe's timings for one test.
func fastPipe(t *testing.T, idle, steady, backoff time.Duration) {
	t.Helper()
	oldIdle, oldSteady, oldBackoff := pipeIdle, pipeSteady, pipeBackoff
	pipeIdle, pipeSteady, pipeBackoff = idle, steady, backoff
	t.Cleanup(func() { pipeIdle, pipeSteady, pipeBackoff = oldIdle, oldSteady, oldBackoff })
}

func liveTrack(url string) *manifest.Stream {
	return &manifest.Stream{Type: manifest.Video, ID: "ts", PlaylistURL: url, Live: true, Segments: []manifest.Segment{{URL: url}}}
}

// The source drops the connection now and then, and it is reopened at once.
// Every connection starts with stray bytes. The sink still gets every packet
// exactly once, in order, never torn, and in chunks of at least pipeChunk
// except where a connection ended.
func TestPipeLiveTS(t *testing.T) {
	fastPipe(t, 5*time.Second, 0, time.Hour) // every connection counts as steady: no waits
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := int(hits.Add(1)) - 1
		w.Header().Set("Content-Type", "video/mp2t")
		w.(http.Flusher).Flush()
		w.Write([]byte("xyz"))
		for i := 0; i < 500; i += 50 {
			w.Write(tsStream(k*500+i, 50))
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	client, _ := httpx.New(httpx.Options{Retries: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &chunkSink{want: 1500, enough: cancel}

	err := PipeLiveTS(ctx, client, liveTrack(srv.URL+"/live.ts"), sink, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PipeLiveTS = %v, want context.Canceled", err)
	}
	wantRun(t, packetNumbers(t, sink.bytes()), 0, 1500)
	if hits.Load() != 3 {
		t.Fatalf("%d connections, want 3", hits.Load())
	}
	// Only the tail a connection leaves when it ends may be under pipeChunk.
	if sink.small > 3 {
		t.Fatalf("%d chunks under %d bytes, want at most one per connection", sink.small, pipeChunk)
	}
}

// Bytes that do not belong to a packet in the middle of a connection are
// dropped, and the packets around them come through whole.
func TestPipeLiveTSResync(t *testing.T) {
	fastPipe(t, 5*time.Second, time.Hour, time.Millisecond)
	body := append(tsStream(0, 100), "garbage"...)
	body = append(body, tsStream(100, 100)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		w.Write(body)
	}))
	defer srv.Close()
	client, _ := httpx.New(httpx.Options{Retries: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &chunkSink{want: 200, enough: cancel}
	PipeLiveTS(ctx, client, liveTrack(srv.URL), sink, nil)
	wantRun(t, packetNumbers(t, sink.bytes()), 0, 200)
}

// A connection that stays open but stops sending is dropped after pipeIdle,
// what it had sent is not held back, and the stream picks up on a new
// connection.
func TestPipeLiveTSStall(t *testing.T) {
	fastPipe(t, 200*time.Millisecond, time.Hour, time.Millisecond)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := int(hits.Add(1)) - 1
		w.(http.Flusher).Flush()
		w.Write(tsStream(k*30, 30)) // less than one chunk, then silence
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	client, _ := httpx.New(httpx.Options{Retries: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &chunkSink{want: 60, enough: cancel}
	done := make(chan error, 1)
	go func() { done <- PipeLiveTS(ctx, client, liveTrack(srv.URL), sink, nil) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PipeLiveTS never dropped the silent connection")
	}
	wantRun(t, packetNumbers(t, sink.bytes()), 0, 60)
	if hits.Load() != 2 {
		t.Fatalf("%d connections, want 2", hits.Load())
	}
}

// A source that sends a few packets and hangs up every time is not given up
// on while it sends media, but it is not hammered either: each short
// connection waits longer before the next one.
func TestPipeLiveTSShortConnections(t *testing.T) {
	fastPipe(t, 5*time.Second, time.Hour, 20*time.Millisecond)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.(http.Flusher).Flush()
		w.Write(tsStream(0, 20))
	}))
	defer srv.Close()
	client, _ := httpx.New(httpx.Options{Retries: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	sink := &chunkSink{}
	if err := PipeLiveTS(ctx, client, liveTrack(srv.URL), sink, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PipeLiveTS = %v, want it to keep going until the deadline", err)
	}
	// Waits of 20, 40, 80, 160 ms (plus jitter) fit about five connections in
	// half a second; reopening at once would make hundreds.
	if n := hits.Load(); n < 2 || n > 8 {
		t.Fatalf("%d connections in 500ms, want a handful", n)
	}
	packetNumbers(t, sink.bytes())
}

// failingSink refuses every chunk, like an output whose muxer has gone away.
type failingSink struct{ err error }

func (s failingSink) Init([]byte) error                        { return nil }
func (s failingSink) Segment(engine.SegmentInfo, []byte) error { return s.err }

// A sink error ends the pipe at once with that error: opening the source
// again cannot fix the output.
func TestPipeLiveTSSinkError(t *testing.T) {
	fastPipe(t, 5*time.Second, 0, time.Millisecond)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		endless(w, r)
	}))
	defer srv.Close()
	client, _ := httpx.New(httpx.Options{Retries: 1})
	broken := errors.New("pipe to the muxer closed")
	err := PipeLiveTS(context.Background(), client, liveTrack(srv.URL), failingSink{broken}, nil)
	if !errors.Is(err, broken) {
		t.Fatalf("PipeLiveTS = %v, want the sink's error", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("%d connections, want 1", hits.Load())
	}
}

// PipeLiveTS gives up after pipeAttempts connections in a row bring no media,
// and says why: a refused URL, or an error page sent with a 200.
func TestPipeLiveTSGivesUp(t *testing.T) {
	fastPipe(t, 5*time.Second, time.Hour, time.Millisecond)
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"refused", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, "HTTP 404"},
		{"error page", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html><body>Not available</body></html>"))
		}, errNotTS.Error()},
	}
	for _, c := range cases {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			c.handler(w, r)
		}))
		client, _ := httpx.New(httpx.Options{Retries: 1})
		err := PipeLiveTS(context.Background(), client, liveTrack(srv.URL), &chunkSink{}, nil)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", c.name, err, c.want)
		}
		if int(hits.Load()) != pipeAttempts {
			t.Errorf("%s: %d connections, want %d", c.name, hits.Load(), pipeAttempts)
		}
	}
}
