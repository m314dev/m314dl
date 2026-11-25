package engine

import "sync/atomic"

// Keyring holds the CENC content keys by KID. It is a pointer that can be
// swapped while a download runs, which is what lets a restream take a rotated
// key without stopping: the next segment to decrypt simply looks the key up
// in the new set. A nil Keyring has no keys.
type Keyring struct {
	m atomic.Pointer[map[[16]byte][]byte]
}

// NewKeyring returns a Keyring holding keys; nil or empty is fine.
func NewKeyring(keys map[[16]byte][]byte) *Keyring {
	k := &Keyring{}
	k.Set(keys)
	return k
}

// Set replaces the whole set of keys.
func (k *Keyring) Set(keys map[[16]byte][]byte) {
	if keys == nil {
		keys = map[[16]byte][]byte{}
	}
	k.m.Store(&keys)
}

// Map returns the current keys. The map must not be modified.
func (k *Keyring) Map() map[[16]byte][]byte {
	if k == nil {
		return nil
	}
	return *k.m.Load()
}
