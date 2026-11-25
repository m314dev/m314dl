package source

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/m314dev/m314dl/internal/engine"
	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
)

// Live is a running channel's input as its download loops see it: the HTTP
// client, the content keys and the current playlist of every selected track.
// All of it can be replaced while the loops run. A refresh reads the track's
// playlist URL from here each time, a segment fetch goes through Client, a
// decrypt looks its key up in Keys, so after Replace the very next refresh
// follows the new manifest and the output carries on in the same sequence.
// That is what lets a channel outlive a manifest URL whose token expires.
type Live struct {
	Kind   string
	Client *httpx.Client // replaced in place with Client.Replace
	Keys   *engine.Keyring

	// Reconnects counts the times the raw TS pipe had to open the source
	// again. Segmented sources do not reconnect; their refreshes fail and
	// retry instead, which Client's error count shows.
	Reconnects atomic.Int64

	mu     sync.RWMutex
	tracks map[string]*manifest.Stream // by stream ID
	gen    int
}

// NewLive holds the input a channel starts with.
func NewLive(kind string, client *httpx.Client, keys map[[16]byte][]byte, selected []*manifest.Stream) *Live {
	l := &Live{Kind: kind, Client: client, Keys: engine.NewKeyring(keys), tracks: map[string]*manifest.Stream{}}
	for _, st := range selected {
		l.tracks[st.ID] = st
	}
	return l
}

// Track returns the current view of the track with this ID, or nil.
func (l *Live) Track(id string) *manifest.Stream {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.tracks[id]
}

// Tracks returns the current view of every track, in no particular order.
func (l *Live) Tracks() []*manifest.Stream {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*manifest.Stream, 0, len(l.tracks))
	for _, st := range l.tracks {
		out = append(out, st)
	}
	return out
}

// Generation counts the replacements so far, starting at 0.
func (l *Live) Generation() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.gen
}

// Replace swaps the input. selected is the new source's selection and must
// contain every track currently running, matched by ID; a track that is
// missing fails the whole call and nothing changes, because a loop with no
// playlist to follow would have to stop. A nil client keeps the current one,
// nil keys keep the current keys. Whether the new tracks are compatible with
// the output (same codec and picture size) is for the caller to check.
func (l *Live) Replace(client *httpx.Client, keys map[[16]byte][]byte, selected []*manifest.Stream) error {
	byID := make(map[string]*manifest.Stream, len(selected))
	for _, st := range selected {
		byID[st.ID] = st
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for id := range l.tracks {
		if byID[id] == nil {
			return fmt.Errorf("track %s is not in the new source", id)
		}
	}
	for id := range l.tracks {
		l.tracks[id] = byID[id]
	}
	if client != nil {
		l.Client.Replace(client)
	}
	if keys != nil {
		l.Keys.Set(keys)
	}
	l.gen++
	return nil
}

// Refresh returns the engine's refresh function for one track. Each call
// re-reads the track's current playlist URL, so a Replace takes effect on the
// next refresh tick with nothing else to do.
func (l *Live) Refresh(id string, logv func(string, ...any)) engine.RefreshFunc {
	return func(ctx context.Context) (*manifest.Stream, error) {
		st := l.Track(id)
		if st == nil {
			return nil, fmt.Errorf("track %s has no source", id)
		}
		return RefreshFunc(l.Client, l.Kind, st, logv)(ctx)
	}
}
