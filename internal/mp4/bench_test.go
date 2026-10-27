package mp4

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkDecryptFragment measures how fast our own CENC and cbcs decryption
// runs on a real fragment packaged by shaka. It reports MB/s so the cost of
// decrypting can be compared with network bandwidth.
func BenchmarkDecryptFragment(b *testing.B) {
	dir := fixtureDir()
	key, _ := hex.DecodeString("0123456789abcdef0123456789abcdef")
	for _, scheme := range []string{"dash-cenc", "dash-cbcs"} {
		sd := filepath.Join(dir, scheme)
		initSeg, err := os.ReadFile(filepath.Join(sd, "v_init.mp4"))
		if err != nil {
			b.Skip("fixtures absent")
		}
		info, err := ParseInit(initSeg)
		if err != nil || info == nil {
			b.Fatalf("parse init: %v", err)
		}
		frag, err := os.ReadFile(filepath.Join(sd, "v_2.m4s"))
		if err != nil {
			b.Fatal(err)
		}
		b.Run(scheme, func(b *testing.B) {
			b.SetBytes(int64(len(frag)))
			buf := make([]byte, len(frag))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				copy(buf, frag)
				if err := DecryptFragment(buf, info, key); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
