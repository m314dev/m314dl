package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/m314dev/m314dl/internal/manifest"
	"github.com/m314dev/m314dl/internal/mp4"
)

// cencDecryptor holds the protection metadata and the content key for one
// stream encrypted with CENC (Common Encryption, the MP4 standard for
// encrypted samples). It is built once per stream, so the init segment is
// parsed once, and then shared read-only by every download worker.
type cencDecryptor struct {
	info *mp4.InitInfo
	key  []byte
}

// decrypt decrypts one CENC or CBCS fragment in place. The work is done in
// this process by the mp4 package, with no external tool.
func (d *cencDecryptor) decrypt(frag []byte) error {
	return mp4.DecryptFragment(frag, d.info, d.key)
}

// streamIsCENC reports whether any segment in the stream is marked as CENC.
func streamIsCENC(st *manifest.Stream) bool {
	for i := range st.Segments {
		if k := st.Segments[i].Key; k != nil && k.Method == manifest.EncCENC {
			return true
		}
	}
	return false
}

// fetchInit downloads the stream's init segment. When the init is a
// SegmentBase-style one that lives inside the media file, it fetches only the
// byte range that holds it.
func fetchInit(ctx context.Context, cfg Config, st *manifest.Stream) ([]byte, error) {
	rng := ""
	if st.Init.Range != nil {
		rng = st.Init.Range.Header()
	}
	initSeg, _, err := cfg.Client.FetchBytes(ctx, st.Init.URL, rng)
	if err != nil {
		return nil, fmt.Errorf("fetch init for %s: %w", st.ID, err)
	}
	return initSeg, nil
}

// newCencDecryptor parses the stream's init segment and then looks up the
// content key. It tries, in order: the default_KID from the init's tenc box,
// then the KID named in the manifest, then a single key supplied without a KID.
//
// It returns (nil, nil) when the init segment shows the stream is not
// encrypted, so the caller can use it both to ask whether decryption is needed
// and to get the decryptor. The init segment has the final say. A manifest can
// leave out ContentProtection while the samples are plainly encrypted. If we
// trusted the manifest alone, the encrypted samples would pass through
// untouched, which plays as a black picture with working audio and reports no
// error anywhere.
func newCencDecryptor(ctx context.Context, cfg Config, st *manifest.Stream, initSeg []byte) (*cencDecryptor, error) {
	info, err := mp4.ParseInit(initSeg)
	if err != nil {
		return nil, fmt.Errorf("parse init for %s: %w", st.ID, err)
	}
	if info == nil {
		// No tenc box. If the manifest claimed protection, the two disagree
		// and that is worth reporting. Otherwise the stream is simply clear.
		if streamIsCENC(st) {
			return nil, fmt.Errorf("stream %s is CENC but its init has no tenc protection box", st.ID)
		}
		return nil, nil
	}

	key := resolveKey(cfg.Keys, info.DefaultKID, manifestKID(st))
	if key == nil {
		// SAMPLE-AES (cbcs) may hand out its raw key through a URI we can
		// fetch, the same way AES-128 does, instead of needing -key.
		if uri := firstFetchableSampleAESURI(st); uri != "" {
			kc := &keyCache{client: cfg.Client}
			k, err := kc.get(ctx, uri)
			if err != nil {
				return nil, fmt.Errorf("fetch SAMPLE-AES key for %s: %w", st.ID, err)
			}
			key = k
		}
	}
	if key == nil {
		return nil, fmt.Errorf("no key for stream %s (KID %x); supply -key %x:<hex-key>",
			st.ID, info.DefaultKID, info.DefaultKID)
	}
	return &cencDecryptor{info: info, key: key}, nil
}

// firstSampleAESKey returns the first SAMPLE-AES key in the stream, or nil.
func firstSampleAESKey(st *manifest.Stream) *manifest.Key {
	for i := range st.Segments {
		if k := st.Segments[i].Key; k != nil && k.Method == manifest.EncSampleAES {
			return k
		}
	}
	return nil
}

// firstFetchableSampleAESURI returns the first SAMPLE-AES key URI that can be
// fetched (http/https/data), or "".
func firstFetchableSampleAESURI(st *manifest.Stream) string {
	if k := firstSampleAESKey(st); k != nil && fetchableKeyURI(k.URI) {
		return k.URI
	}
	return ""
}

// fetchableKeyURI reports whether a key URI points at bytes we can download.
// An skd:// URI or an empty one does not; its key must come from -key.
func fetchableKeyURI(uri string) bool {
	return strings.HasPrefix(uri, "http://") ||
		strings.HasPrefix(uri, "https://") ||
		strings.HasPrefix(uri, "data:")
}

// sampleAESKey finds the content key for a transport-stream SAMPLE-AES
// segment. It uses a fetchable URI when there is one (as AES-128 does).
// Otherwise it uses the key the user gave with -key, matched by KID or taken
// as the single bare key.
func sampleAESKey(ctx context.Context, kc *keyCache, keys map[[16]byte][]byte, k *manifest.Key) ([]byte, error) {
	if fetchableKeyURI(k.URI) {
		return kc.get(ctx, k.URI)
	}
	if key := resolveKey(keys, k.KID); key != nil {
		return key, nil
	}
	return nil, fmt.Errorf("no key for SAMPLE-AES segment; supply the content key with -key <hex-key>")
}

// resolveKey looks up a content key by any of the given KIDs. If none match
// and the only key supplied has no KID, it returns that key.
func resolveKey(keys map[[16]byte][]byte, kids ...[16]byte) []byte {
	for _, kid := range kids {
		if k, ok := keys[kid]; ok {
			return k
		}
	}
	// Convenience for one key: when the only key was given without a KID, use
	// it whatever the KID is.
	var zero [16]byte
	if k, ok := keys[zero]; ok && len(keys) == 1 {
		return k
	}
	return nil
}

func manifestKID(st *manifest.Stream) [16]byte {
	for i := range st.Segments {
		if k := st.Segments[i].Key; k != nil && k.KID != ([16]byte{}) {
			return k.KID
		}
	}
	return [16]byte{}
}
