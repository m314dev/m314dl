// benchserver serves the fixture streams and can imitate network conditions,
// so downloader benchmarks can be repeated with the same results.
//
//	benchserver -root ../streams -port 8090 -latency 30ms -rate 2000000
//
// -latency: delay before the first byte of each response (imitates the RTT,
// the round-trip time of a request)
// -rate: cap on bytes per second for each response (0 means unlimited)
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	root := flag.String("root", ".", "directory to serve")
	port := flag.Int("port", 8090, "listen port")
	latency := flag.Duration("latency", 0, "per-request first-byte delay")
	rate := flag.Int64("rate", 0, "per-response bytes/sec cap (0 = unlimited)")
	flag.Parse()

	fs := http.FileServer(http.Dir(*root))
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *latency > 0 {
			time.Sleep(*latency)
		}
		if *rate > 0 {
			w = &throttled{ResponseWriter: w, rate: *rate, last: time.Now()}
		}
		fs.ServeHTTP(w, r)
	})
	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	fmt.Fprintf(os.Stderr, "benchserver on http://%s root=%s latency=%s rate=%d B/s\n", addr, *root, *latency, *rate)
	if err := http.ListenAndServe(addr, h); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// throttled is a token-bucket writer. It sleeps as needed so the response
// averages rate bytes per second.
type throttled struct {
	http.ResponseWriter
	rate   int64
	bucket int64
	last   time.Time
}

func (t *throttled) Write(p []byte) (int, error) {
	const chunk = 64 << 10
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > chunk {
			n = chunk
		}
		now := time.Now()
		t.bucket += int64(now.Sub(t.last).Seconds() * float64(t.rate))
		if t.bucket > t.rate { // cap a burst at one second's worth
			t.bucket = t.rate
		}
		t.last = now
		if t.bucket < int64(n) {
			deficit := int64(n) - t.bucket
			time.Sleep(time.Duration(float64(deficit) / float64(t.rate) * float64(time.Second)))
			t.bucket = 0
		} else {
			t.bucket -= int64(n)
		}
		m, err := t.ResponseWriter.Write(p[:n])
		written += m
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}
