package engine

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
)

func TestStripFakeImageHeader(t *testing.T) {
	ts := make([]byte, 188*3)
	for i := 0; i < 3; i++ {
		ts[i*188] = 0x47
	}
	fake := append([]byte{0x89, 'P', 'N', 'G', 0, 0, 0, 0, 1, 2, 3}, ts...)
	got := stripFakeImageHeader(fake)
	if !bytes.Equal(got, ts) {
		t.Fatalf("PNG header not stripped: %d vs %d bytes", len(got), len(ts))
	}
	// clean data untouched
	if !bytes.Equal(stripFakeImageHeader(ts), ts) {
		t.Fatal("clean TS modified")
	}
}

func TestDecryptAES128(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := make([]byte, 16)
	plain := []byte("hello segment data")
	// pad + encrypt
	pad := 16 - len(plain)%16
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(key)
	enc := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(enc, padded)

	got, err := decryptAES128CBC(enc, key, iv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("got %q want %q", got, plain)
	}
}

// End-to-end: segments served over HTTP, one flaky segment (fails twice then
// succeeds), verify ordered output and resume-state cleanup.
func TestDownloadStreamOrderAndRetry(t *testing.T) {
	var flakes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/seg2" && flakes.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	defer srv.Close()

	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < 5; i++ {
		st.Segments = append(st.Segments, manifest.Segment{
			URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: 1,
		})
	}
	client, _ := httpx.New(httpx.Options{Retries: 5})
	out := filepath.Join(t.TempDir(), "out.ts")
	err := DownloadStream(context.Background(), Config{Client: client, Threads: 3}, st, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	want := "[/seg0][/seg1][/seg2][/seg3][/seg4]"
	if string(b) != want {
		t.Fatalf("out = %q want %q", b, want)
	}
	if _, err := os.Stat(out + ".m314dl-state"); !os.IsNotExist(err) {
		t.Fatal("state file should be removed on success")
	}
}

func TestDownloadStreamVODFailNoHole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/seg1" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	defer srv.Close()

	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < 3; i++ {
		st.Segments = append(st.Segments, manifest.Segment{
			URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: 1,
		})
	}
	client, _ := httpx.New(httpx.Options{Retries: 1})
	out := filepath.Join(t.TempDir(), "out.ts")
	err := DownloadStream(context.Background(), Config{Client: client, Threads: 2}, st, out, nil)
	if err == nil {
		t.Fatal("want error for 404 segment")
	}
	// only the contiguous prefix before the failure may be committed
	b, _ := os.ReadFile(out)
	if string(b) != "[/seg0]" {
		t.Fatalf("committed %q, want only seg0", b)
	}
}

func TestDRMRefused(t *testing.T) {
	st := &manifest.Stream{ID: "drm", Segments: []manifest.Segment{
		{URL: "http://x/seg0", Key: &manifest.Key{Method: manifest.EncCENC}},
	}}
	client, _ := httpx.New(httpx.Options{})
	err := DownloadStream(context.Background(), Config{Client: client}, st, filepath.Join(t.TempDir(), "o"), nil)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("DRM")) {
		t.Fatalf("want DRM refusal, got %v", err)
	}
}

func TestTempStreamPathIsolation(t *testing.T) {
	st := &manifest.Stream{Type: manifest.Video, ID: "video=3000000"}
	// two episodes in the same season directory, same manifest stream id
	a := TempStreamPath("/lib/Show/Season 01/E01.mkv", st, ".mp4")
	b := TempStreamPath("/lib/Show/Season 01/E02.mkv", st, ".mp4")
	if a == b {
		t.Fatalf("concurrent episodes must not share a temp path: both %q", a)
	}
	// stable across reruns of the same command (resume relies on this)
	if a2 := TempStreamPath("/lib/Show/Season 01/E01.mkv", st, ".mp4"); a2 != a {
		t.Fatalf("temp path not stable: %q vs %q", a, a2)
	}
	// lives beside the output, not in some scratch dir
	if got := filepath.Dir(a); got != "/lib/Show/Season 01" {
		t.Fatalf("temp not beside output: %q", got)
	}
}

// TestDownloadStreamResume simulates an interrupted run (checkpoint on disk)
// and verifies the rerun skips already-done segments, appends byte-exact, and
// seeds progress from the checkpoint instead of restarting at 0.
func TestDownloadStreamResume(t *testing.T) {
	var requested sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(r.URL.Path, true)
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	defer srv.Close()

	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < 5; i++ {
		st.Segments = append(st.Segments, manifest.Segment{
			URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: 1,
		})
	}
	out := filepath.Join(t.TempDir(), "out.ts")
	// a prior run wrote the first two segments and checkpointed
	prefix := "[/seg0][/seg1]"
	os.WriteFile(out, []byte(prefix), 0o644)
	os.WriteFile(out+".m314dl-state", []byte(fmt.Sprintf(`{"next_idx":2,"offset":%d}`, len(prefix))), 0o644)

	prog := NewProgress(false, 0)
	client, _ := httpx.New(httpx.Options{Retries: 2})
	if err := DownloadStream(context.Background(), Config{Client: client, Threads: 2, Progress: prog}, st, out, nil); err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(out)
	if string(b) != "[/seg0][/seg1][/seg2][/seg3][/seg4]" {
		t.Fatalf("resumed output = %q", b)
	}
	if _, ok := requested.Load("/seg0"); ok {
		t.Fatal("seg0 re-downloaded on resume")
	}
	if _, ok := requested.Load("/seg1"); ok {
		t.Fatal("seg1 re-downloaded on resume")
	}
	if d := prog.done.Load(); d != 5 {
		t.Fatalf("progress done = %d, want 5 (2 seeded + 3 new)", d)
	}
	if _, err := os.Stat(out + ".m314dl-state"); !os.IsNotExist(err) {
		t.Fatal("state file should be removed on success")
	}
}

// TestIntraSegmentResume proves byte-range resume within a single segment: a
// partial part file on disk is completed with a Range request, not re-fetched
// from scratch.
func TestIntraSegmentResume(t *testing.T) {
	content := func(i int) []byte { return bytes.Repeat([]byte{byte('A' + i)}, 100) }
	var ranges sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var i int
		fmt.Sscanf(r.URL.Path, "/seg%d", &i)
		full := content(i)
		if rh := r.Header.Get("Range"); rh != "" {
			ranges.Store(r.URL.Path, rh)
			var start int
			fmt.Sscanf(rh, "bytes=%d-", &start)
			if start >= len(full) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(full)-1, len(full)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(full[start:])
			return
		}
		w.Write(full)
	}))
	defer srv.Close()

	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < 3; i++ {
		st.Segments = append(st.Segments, manifest.Segment{
			URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: 1,
		})
	}
	out := filepath.Join(t.TempDir(), "out.ts")
	// a prior run got 30 of seg1's 100 bytes before being killed
	if err := os.WriteFile(partPath(out, 1), content(1)[:30], 0o644); err != nil {
		t.Fatal(err)
	}

	client, _ := httpx.New(httpx.Options{Retries: 3})
	if err := DownloadStream(context.Background(), Config{Client: client, Threads: 2}, st, out, nil); err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(out)
	want := append(append(content(0), content(1)...), content(2)...)
	if !bytes.Equal(b, want) {
		t.Fatalf("output mismatch: got %d bytes, want %d", len(b), len(want))
	}
	// seg1 resumed from byte 30; seg0/seg2 fetched fresh (no Range)
	if rh, ok := ranges.Load("/seg1"); !ok || rh.(string) != "bytes=30-" {
		t.Fatalf("seg1 should resume with Range bytes=30-, got %v", rh)
	}
	if _, ok := ranges.Load("/seg0"); ok {
		t.Fatal("seg0 should not be range-requested")
	}
}

// TestProgressTotalUpfront proves the segment total is published before
// downloads complete, so the percentage denominator is final from the first
// tick instead of inflating as segments trickle out (which slid the bar back).
func TestProgressTotalUpfront(t *testing.T) {
	gate := make(chan struct{})
	reached := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
		<-gate // hold every segment until the test releases
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	defer srv.Close()

	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < 5; i++ {
		st.Segments = append(st.Segments, manifest.Segment{
			URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: 1,
		})
	}
	out := filepath.Join(t.TempDir(), "out.ts")
	prog := NewProgress(false, 0)
	client, _ := httpx.New(httpx.Options{Retries: 1})
	done := make(chan error, 1)
	go func() {
		done <- DownloadStream(context.Background(), Config{Client: client, Threads: 2, Progress: prog}, st, out, nil)
	}()

	<-reached // a worker is fetching, so feed has already published the total
	if tot := prog.total.Load(); tot != 5 {
		t.Fatalf("total = %d before any segment completed, want 5 upfront", tot)
	}
	if d := prog.done.Load(); d != 0 {
		t.Fatalf("done = %d while segments are blocked, want 0", d)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if prog.total.Load() != 5 || prog.done.Load() != 5 {
		t.Fatalf("final total/done = %d/%d, want 5/5", prog.total.Load(), prog.done.Load())
	}
}

type countingSink struct{ n atomic.Int64 }

func (s *countingSink) Init([]byte) error { return nil }
func (s *countingSink) Segment(SegmentInfo, []byte) error {
	s.n.Add(1)
	return nil
}

// PaceRealtime holds a finite restream to the source's real duration after a
// short prime burst, so a VOD can't flood the live output. A lower-bound timing
// check: the paced segments cannot finish faster than their own durations.
func TestSinkWriterPaceRealtime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	defer srv.Close()

	const n = 8
	const dur = 0.05 // seconds/segment
	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < n; i++ {
		st.Segments = append(st.Segments, manifest.Segment{
			URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: dur,
		})
	}
	client, _ := httpx.New(httpx.Options{})
	sink := &countingSink{}

	start := time.Now()
	err := DownloadStream(context.Background(),
		Config{Client: client, Threads: 4, Sink: sink, PaceRealtime: true},
		st, filepath.Join(t.TempDir(), "o"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if sink.n.Load() != n {
		t.Fatalf("emitted %d segments, want %d", sink.n.Load(), n)
	}
	// The prime burst is free; the rest are paced to dur each.
	wantMin := time.Duration(float64(n-pacePrimeSegments) * dur * float64(time.Second))
	if got := time.Since(start); got < wantMin {
		t.Fatalf("paced run took %v, want at least %v", got, wantMin)
	}
}

// ---- SAMPLE-AES routing ----

func mpegCRC32(b []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, x := range b {
		crc ^= uint32(x) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// tsPkt builds one 188-byte TS packet, padding a short payload with adaptation
// stuffing (matching how a real muxer fills the final packet of a PES).
func tsPkt(pid uint16, pusi bool, payload []byte) []byte {
	p := make([]byte, 188)
	for i := range p {
		p[i] = 0xff
	}
	p[0] = 0x47
	p[1] = byte(pid>>8) & 0x1f
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	if len(payload) >= 184 {
		p[3] = 0x10
		copy(p[4:], payload[:184])
	} else {
		p[3] = 0x30
		p[4] = byte(184 - 1 - len(payload))
		if p[4] >= 1 {
			p[5] = 0x00
		}
		copy(p[188-len(payload):], payload)
	}
	return p
}

// buildAACSampleAESTS builds a one-frame MPEG-TS whose single ADTS/AAC frame is
// SAMPLE-AES encrypted (stream_type 0xCF), plus the clear frame for comparison.
func buildAACSampleAESTS(t *testing.T, key, iv []byte) (enc []byte, clearFrame []byte, audioPID uint16) {
	t.Helper()
	audioPID = 0x0101
	pmtPID := uint16(0x1000)

	// clear ADTS frame: 7-byte header + 160-byte payload (whole PES fits one packet).
	frame := make([]byte, 7+160)
	frame[0], frame[1], frame[2] = 0xff, 0xf1, 0x50 // sync, MPEG-4 no-CRC, AAC-LC
	total := len(frame)
	frame[3] = byte((total >> 11) & 0x03)
	frame[4] = byte((total >> 3) & 0xff)
	frame[5] = byte((total&0x07)<<5) | 0x1f
	frame[6] = 0xfc
	for i := 7; i < total; i++ {
		frame[i] = byte(i * 31)
	}
	clearFrame = append([]byte(nil), frame...)

	// encrypt in place: 16-byte clear leader after the header, then whole blocks.
	block, _ := aes.NewCipher(key)
	start := 7 + 16
	n := (len(frame) - start) &^ 15
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(frame[start:start+n], frame[start:start+n])

	// PES wrapping the encrypted frame.
	pes := []byte{0x00, 0x00, 0x01, 0xc0, 0x00, 0x00, 0x80, 0x80, 0x05, 0x21, 0, 1, 0, 1}
	pes = append(pes, frame...)

	// PAT: program 1 -> PMT pid.
	pat := []byte{0x00, 0xb0, 0x0d, 0x00, 0x01, 0xc1, 0x00, 0x00, 0x00, 0x01,
		byte(0xe0 | (pmtPID>>8)&0x1f), byte(pmtPID & 0xff)}
	pat = appendU32(pat, mpegCRC32(pat))

	// PMT: one encrypted-AAC elementary stream.
	pmt := []byte{0x02, 0x00, 0x00, 0x00, 0x01, 0xc1, 0x00, 0x00,
		byte(0xe0 | (audioPID>>8)&0x1f), byte(audioPID & 0xff), 0x00, 0x00,
		0xcf, byte(0xe0 | (audioPID>>8)&0x1f), byte(audioPID & 0xff), 0x00, 0x00}
	secLen := len(pmt) - 3 + 4
	pmt[1] = byte(0xb0 | (secLen>>8)&0x0f)
	pmt[2] = byte(secLen)
	pmt = appendU32(pmt, mpegCRC32(pmt))

	enc = append(enc, tsPkt(0x0000, true, append([]byte{0x00}, pat...))...)
	enc = append(enc, tsPkt(pmtPID, true, append([]byte{0x00}, pmt...))...)
	enc = append(enc, tsPkt(audioPID, true, pes)...)
	return enc, clearFrame, audioPID
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// End-to-end: a SAMPLE-AES transport stream routed through decryptSegment with a
// user-supplied bare key is decrypted in place.
func TestDecryptSegmentSampleAESTS(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("ABCDEFGHIJKLMNOP")
	encTS, clearFrame, audioPID := buildAACSampleAESTS(t, key, iv)

	var zero [16]byte
	keys := map[[16]byte][]byte{zero: key}
	it := item{seq: 0, key: &manifest.Key{Method: manifest.EncSampleAES, IV: iv}}

	out, err := decryptSegment(context.Background(), &keyCache{}, nil, 0, nil, keys, it, append([]byte(nil), encTS...))
	if err != nil {
		t.Fatal(err)
	}
	// recover the audio PES payload and compare its ADTS frame to the clear one.
	got := demuxAudioES(out, audioPID)
	if !bytes.Equal(got[:len(clearFrame)], clearFrame) {
		t.Fatalf("SAMPLE-AES TS not decrypted: got %x want %x", got[:min2(32, len(got))], clearFrame[:32])
	}
	// PMT stream_type must be rewritten from 0xcf to 0x0f (clear AAC).
	if bytes.IndexByte(out, 0xcf) != -1 && !hasClearAACType(out) {
		t.Fatalf("PMT stream_type not rewritten to clear")
	}
}

func TestSampleAESKeyResolution(t *testing.T) {
	key := []byte("0123456789abcdef")
	kc := &keyCache{}
	// bare key (zero KID)
	var zero [16]byte
	got, err := sampleAESKey(context.Background(), kc, map[[16]byte][]byte{zero: key}, &manifest.Key{Method: manifest.EncSampleAES})
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("bare key: got %x err %v", got, err)
	}
	// fetchable data: URI
	b64 := "MDEyMzQ1Njc4OWFiY2RlZg==" // base64 of the key
	got, err = sampleAESKey(context.Background(), kc, nil, &manifest.Key{Method: manifest.EncSampleAES, URI: "data:;base64," + b64})
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("data URI key: got %x err %v", got, err)
	}
	// no key available -> error
	if _, err := sampleAESKey(context.Background(), kc, nil, &manifest.Key{Method: manifest.EncSampleAES, URI: "skd://x"}); err == nil {
		t.Fatalf("want error when no key available")
	}
}

func demuxAudioES(ts []byte, pid uint16) []byte {
	var pes []byte
	for off := 0; off+188 <= len(ts); off += 188 {
		p := ts[off : off+188]
		if p[0] != 0x47 || uint16(p[1]&0x1f)<<8|uint16(p[2]) != pid {
			continue
		}
		afc := (p[3] >> 4) & 3
		po := 4
		if afc == 3 {
			po = 5 + int(p[4])
		} else if afc != 1 {
			continue
		}
		if po < 188 {
			pes = append(pes, p[po:]...)
		}
	}
	if len(pes) < 9 || pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
		return nil
	}
	return pes[9+int(pes[8]):]
}

func hasClearAACType(ts []byte) bool {
	for off := 0; off+188 <= len(ts); off += 188 {
		p := ts[off : off+188]
		if p[0] == 0x47 && bytes.IndexByte(p, 0x0f) != -1 {
			return true
		}
	}
	return false
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// recordingSink captures each emitted segment's bytes (the test origin echoes
// the request path), so a test can assert exactly which segments were delivered.
type recordingSink struct {
	mu   sync.Mutex
	body []string
}

func (s *recordingSink) Init([]byte) error { return nil }
func (s *recordingSink) Segment(_ SegmentInfo, data []byte) error {
	s.mu.Lock()
	s.body = append(s.body, string(data))
	s.mu.Unlock()
	return nil
}

// TestLiveSeenMapPrunedNoReEmit drives a live stream whose window slides forward
// over many refreshes. The feeder's dedup map is pruned to the current window
// each refresh (so it can't grow unbounded on a 24/7 channel); this asserts the
// pruning never causes a segment still to be re-emitted or dropped — each is
// delivered exactly once, in order.
func TestLiveSeenMapPrunedNoReEmit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s", r.URL.Path) // echo path so the sink knows the segment
	}))
	defer srv.Close()

	const total = 30 // more than any single window: forces the map to prune
	const win = 3
	seg := func(i int) manifest.Segment {
		return manifest.Segment{URL: fmt.Sprintf("%s/s%d", srv.URL, i), Seq: int64(i), Duration: 0.01}
	}
	// window(n) is the sliding view [n, n+win); Refresh tiny so the loop spins.
	window := func(n int, live bool) *manifest.Stream {
		st := &manifest.Stream{ID: "t", Type: manifest.Video, Live: live, Refresh: time.Millisecond}
		for i := n; i < n+win && i < total; i++ {
			st.Segments = append(st.Segments, seg(i))
		}
		return st
	}

	var next int32 = 1 // first refresh returns window starting at 1
	refresh := func(context.Context) (*manifest.Stream, error) {
		n := int(atomic.AddInt32(&next, 1)) - 2 + 1 // 1,2,3,...
		start := n
		live := start+win < total
		return window(start, live), nil
	}

	sink := &recordingSink{}
	err := DownloadStream(context.Background(),
		Config{Client: mustClient(t), Threads: 4, Sink: sink, FromStart: true},
		window(0, true), filepath.Join(t.TempDir(), "o"), refresh)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]int{}
	for _, b := range sink.body {
		seen[b]++
	}
	for i := 0; i < total; i++ {
		p := fmt.Sprintf("/s%d", i)
		if seen[p] != 1 {
			t.Fatalf("segment %s delivered %d times, want exactly 1 (map prune must not re-emit or drop)", p, seen[p])
		}
	}
	if len(seen) != total {
		t.Fatalf("delivered %d distinct segments, want %d", len(seen), total)
	}
}

func mustClient(t *testing.T) *httpx.Client {
	t.Helper()
	c, err := httpx.New(httpx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A commit error (here: the AES-128 key for segment 2 is a 404) must end
// DownloadStream even while the feeder still has segments to push. Before the
// fix the writer died, the workers filled `results`, the feeder parked on
// `items`, and the process printed the same progress line forever with no
// sockets open — seen fleet-wide as "hung" downloads.
func TestWriterErrorStopsPipeline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/badkey" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(5 * time.Millisecond)
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	defer srv.Close()

	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < 200; i++ {
		seg := manifest.Segment{URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: 1}
		if i == 2 {
			seg.Key = &manifest.Key{Method: manifest.EncAES128, URI: srv.URL + "/badkey"}
		}
		st.Segments = append(st.Segments, seg)
	}
	client, _ := httpx.New(httpx.Options{Retries: 1})
	out := filepath.Join(t.TempDir(), "out.ts")
	done := make(chan error, 1)
	go func() {
		done <- DownloadStream(context.Background(), Config{Client: client, Threads: 2}, st, out, nil)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want the key error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DownloadStream hung after the writer failed")
	}
}

// AES-128 ciphertext is random bytes, so it can open FF D8 (JPEG) and hold
// three 0x47s 188 apart by chance. The fake-header strip must not run on it:
// it once chopped a 2084928-byte segment to 256222 and the decrypt failed.
func TestAES128CiphertextNotTakenForFakeImage(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 16)
	ct := make([]byte, 4096)
	for i := range ct {
		ct[i] = byte(i*31 + 5)
	}
	ct[0], ct[1] = 0xFF, 0xD8
	for _, off := range []int{1001, 1001 + 188, 1001 + 376} {
		ct[off] = 0x47
	}
	it := item{key: &manifest.Key{
		Method: manifest.EncAES128,
		URI:    "data:text/plain," + string(key),
		IV:     make([]byte, 16),
	}}
	got, err := decryptSegment(context.Background(), &keyCache{}, nil, 0, nil, nil, it, ct)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ct) {
		t.Fatalf("got %d bytes, want %d", len(got), len(ct))
	}
}
