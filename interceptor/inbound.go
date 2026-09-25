package main

// Inbound side: reverse proxy in front of the function. containerConcurrency
// is 1, so a second concurrent request is a bug.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

type invocation struct {
	id string

	mu          sync.Mutex // guards seqs and the seq==list-index invariant (see logAttempt)
	seq         int
	deferredSeq int

	pending []pendingWrite
	// Mirrored in Redis for the reverter; only this process ever sends them.
	deferred []deferredCall

	failed bool // proxy could not reach the function, it likely just died
}

func (inv *invocation) queue(w pendingWrite) {
	inv.mu.Lock()
	inv.pending = append(inv.pending, w)
	inv.mu.Unlock()
}

func (inv *invocation) takePending() []pendingWrite {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	ws := inv.pending
	inv.pending = nil
	return ws
}

type interceptor struct {
	store  *store
	pod    string
	tier0  []tier0Rule
	egress *http.Client

	mu      sync.Mutex
	current *invocation
}

// Sortable prefix plus random suffix, a stdlib stand-in for a ULID.
func newInvID() string {
	var b [4]byte
	rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405.000") + "-" + hex.EncodeToString(b[:])
}

func (ic *interceptor) begin(ctx context.Context) (*invocation, error) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	if ic.current != nil {
		return nil, fmt.Errorf("invocation %s already in flight; is containerConcurrency=1 set?", ic.current.id)
	}
	inv := &invocation{id: newInvID()}
	// Fail closed: an invocation that cannot be recorded must not run.
	if err := ic.store.MarkRunning(ctx, ic.pod, inv.id); err != nil {
		return nil, fmt.Errorf("effect store unavailable: %w", err)
	}
	ic.current = inv
	return inv, nil
}

func (ic *interceptor) abort(inv *invocation, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), detachedWriteTimeout)
	defer cancel()
	log.Printf("invocation %s: ABORT, %s", inv.id, reason)
	if err := ic.store.SelfAbort(ctx, inv.takePending(), ic.pod, inv.id); err != nil {
		log.Printf("invocation %s: self-abort mark failed: %v", inv.id, err)
	}
	ic.clearCurrent()
}

// end decides the commit while the response is still buffered, so the 200
// the client sees already includes the deferred calls.
func (ic *interceptor) end(w http.ResponseWriter, buf *bufferedResponse, inv *invocation) {
	if inv.failed {
		// Our own exit follows. ic.current stays set so a stray request gets 503.
		log.Printf("invocation %s: function unreachable, leaving crash state for detector", inv.id)
		buf.release(w)
		return
	}
	if !buf.ok() {
		ic.abort(inv, fmt.Sprintf("function returned %d", buf.status))
		buf.release(w)
		return
	}
	if err := ic.flushDeferred(inv); err != nil {
		ic.abort(inv, "deferred flush failed: "+err.Error())
		http.Error(w, "deferred effect failed; invocation will be compensated", http.StatusBadGateway)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), detachedWriteTimeout)
	defer cancel()
	if err := ic.store.MarkCommitted(ctx, inv.takePending(), ic.pod, inv.id); err != nil {
		log.Printf("invocation %s: commit mark failed: %v", inv.id, err)
	}
	ic.clearCurrent()
	buf.release(w)
}

func (ic *interceptor) clearCurrent() {
	ic.mu.Lock()
	ic.current = nil
	ic.mu.Unlock()
}

func (ic *interceptor) peek() *invocation {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	return ic.current
}

func isProbe(r *http.Request) bool {
	return r.Header.Get("K-Kubelet-Probe") != "" ||
		r.Header.Get("K-Network-Probe") != "" ||
		r.URL.Path == "/healthz"
}

type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferedResponse() *bufferedResponse {
	return &bufferedResponse{header: make(http.Header), status: http.StatusOK}
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) WriteHeader(code int)        { b.status = code }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedResponse) ok() bool                    { return b.status >= 200 && b.status < 300 }

// The function's hop-by-hop headers describe the loopback link, not the caller's.
func (b *bufferedResponse) release(w http.ResponseWriter) {
	copyRespHeaders(w.Header(), b.header)
	w.WriteHeader(b.status)
	w.Write(b.body.Bytes())
}

func (ic *interceptor) inboundHandler() http.Handler {
	target, _ := url.Parse("http://" + functionAddr)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// A canceled inbound context means the client left, not that the function
		// died: leave failed unset so end() aborts and frees the slot.
		if r.Context().Err() == nil {
			if inv := ic.peek(); inv != nil {
				inv.failed = true
			}
		}
		log.Printf("inbound: proxy error: %v", err)
		w.WriteHeader(http.StatusBadGateway)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProbe(r) {
			proxy.ServeHTTP(w, r)
			return
		}
		inv, err := ic.begin(r.Context())
		if err != nil {
			log.Printf("inbound: REJECTED: %v", err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Atomic-Invocation", inv.id)
		buf := newBufferedResponse()
		proxy.ServeHTTP(buf, r)
		ic.end(w, buf, inv)
	})
}
