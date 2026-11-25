package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/m314dev/m314dl/internal/httpx"
	"github.com/m314dev/m314dl/internal/manifest"
	"github.com/m314dev/m314dl/internal/source"
)

// patchReq is the body of PATCH /api/channels/{id}. Every field is optional
// and an omitted one keeps its current value. Only the input can change: the
// output (format, transcode, selection, idle) is the presentation viewers are
// playing, and changing it means a new channel.
type patchReq struct {
	URL      *string           `json:"url"`
	Keys     []string          `json:"keys"`
	Headers  map[string]string `json:"headers"`
	Proxy    *string           `json:"proxy"`
	Insecure *bool             `json:"insecure"`
}

// patchTimeout bounds the check of the new source: one manifest fetch and the
// selection, which is what POST does too.
const patchTimeout = 20 * time.Second

// apiPatch replaces a running channel's input in place: the manifest URL, the
// content keys, the headers, the proxy, the TLS check. Providers sign their
// manifest URLs with a short expiry and rotate keys on a timer. Stopping the
// channel and starting it again would publish a new presentation from media
// sequence zero, and every player would see its numbering jump back and
// stall. Here the packager and its sequence are untouched: the new source is
// loaded and checked first, and if its tracks match the ones playing, the
// download loops are pointed at it and pick it up on their next refresh. The
// viewers see nothing.
//
// A channel that failed or stopped has no loop left to follow the change and
// is refused. An on-demand channel that is idle takes the new input for its
// next start.
func (w *workerServer) apiPatch(rw http.ResponseWriter, r *http.Request) {
	ch := w.channel(r.PathValue("id"))
	if ch == nil {
		http.Error(rw, "no such channel", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // format, transcode, sv/sa/ss, idle: output settings, not patchable
	var req patchReq
	if err := dec.Decode(&req); err != nil {
		http.Error(rw, `bad request: want {"url":"...","keys":[...],"headers":{...},"proxy":"...","insecure":false}; `+
			`only the input can change, a new output needs a new channel: `+err.Error(), http.StatusBadRequest)
		return
	}
	if req.URL != nil && *req.URL == "" {
		http.Error(rw, "url must not be empty", http.StatusBadRequest)
		return
	}

	state := ch.view().State
	switch state {
	case "running", "idle", "starting":
	default:
		http.Error(rw, "channel is "+state+", not running: start a new one instead", http.StatusConflict)
		return
	}

	// The input as it would be after the change.
	ch.mu.Lock()
	next := ch.req
	keys := ch.keys
	current := ch.selected
	ch.mu.Unlock()
	if req.URL != nil {
		next.URL = *req.URL
	}
	if req.Keys != nil {
		parsed, err := source.ParseKeys(req.Keys)
		if err != nil {
			http.Error(rw, "keys: "+err.Error(), http.StatusBadRequest)
			return
		}
		next.Keys, keys = req.Keys, parsed
	}
	if req.Headers != nil {
		next.Headers = req.Headers
	}
	if req.Proxy != nil {
		next.Proxy = *req.Proxy
	}
	if req.Insecure != nil {
		next.Insecure = *req.Insecure
	}

	// A changed header set, proxy or TLS setting needs a new transport. The
	// check below runs through it, so a proxy that does not work fails here
	// and not in the download loops.
	client := ch.client
	var fresh *httpx.Client
	if req.Headers != nil || req.Proxy != nil || req.Insecure != nil {
		c, err := newClient(next)
		if err != nil {
			http.Error(rw, "client: "+err.Error(), http.StatusBadRequest)
			return
		}
		client, fresh = c, c
	}

	ctx, cancel := context.WithTimeout(r.Context(), patchTimeout)
	defer cancel()
	kind, sel, err := w.resolve(ctx, client, next.URL, keys, ch.ve, ch.ae, ch.se)
	if err != nil {
		http.Error(rw, "new source: "+err.Error(), http.StatusBadRequest)
		return
	}
	if kind != ch.kind {
		http.Error(rw, fmt.Sprintf("new source is %s, the channel plays %s", kind, ch.kind), http.StatusConflict)
		return
	}
	if err := compatible(current, sel); err != nil {
		http.Error(rw, "new source: "+err.Error()+"; a different output needs a new channel", http.StatusConflict)
		return
	}

	// Take it. The loops read the input on their next tick; an idle on-demand
	// channel reads req at its next start.
	if in := ch.in.Load(); in != nil {
		var newKeys map[[16]byte][]byte
		if req.Keys != nil {
			newKeys = keys
		}
		if err := in.Replace(fresh, newKeys, sel); err != nil {
			http.Error(rw, "new source: "+err.Error(), http.StatusConflict)
			return
		}
	} else if fresh != nil {
		ch.client.Replace(fresh)
	}
	ch.mu.Lock()
	ch.req, ch.keys, ch.selected = next, keys, sel
	ch.swaps++
	n := ch.swaps
	ch.mu.Unlock()
	ch.logf("input replaced (%d so far)", n)
	httpx.WriteJSON(rw, ch.view())
}

// compatible checks that a new selection can feed the tracks already playing:
// every current track is there under the same ID, of the same type, codec and
// picture size. The TS and DASH outputs cannot change their init mid-stream,
// and even where HLS could, a viewer asked for this rendition.
func compatible(current, next []*manifest.Stream) error {
	byID := make(map[string]*manifest.Stream, len(next))
	for _, st := range next {
		byID[st.ID] = st
	}
	for _, cur := range current {
		st := byID[cur.ID]
		if st == nil {
			return fmt.Errorf("track %s is missing", cur.ID)
		}
		if st.Type != cur.Type {
			return fmt.Errorf("track %s is %s, was %s", cur.ID, st.Type, cur.Type)
		}
		if st.Codecs != cur.Codecs {
			return fmt.Errorf("track %s codec is %q, was %q", cur.ID, st.Codecs, cur.Codecs)
		}
		if st.Width != cur.Width || st.Height != cur.Height {
			return fmt.Errorf("track %s is %s, was %s", cur.ID, st.Resolution(), cur.Resolution())
		}
	}
	return nil
}
