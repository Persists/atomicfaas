package main

// Outbound side: the proxy that writes the effect log. Requests arrive in origin
// form with the real destination in X-Atomic-Host, set by the shim.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

const (
	captureLimit = 4096
	maxBodyBytes = 1 << 20 // bigger bodies are refused instead of truncated

	detachedWriteTimeout = 2 * time.Second // finish Redis writes after the request context is gone

	// Load-bearing: an egress call without a deadline wedges ic.current forever
	// while the pod stays healthy. Safe: the effect is already logged "attempted".
	egressTimeout = 30 * time.Second
)

func resolveTarget(r *http.Request) (*url.URL, error) {
	if h := r.Header.Get("X-Atomic-Host"); h != "" {
		u := *r.URL
		u.Scheme, u.Host = "http", h
		if r.Header.Get("X-Atomic-Scheme") == "https" {
			u.Scheme = "https" // shim saw the plaintext pre-TLS; we redo TLS to the origin
		}
		return &u, nil
	}
	return nil, errors.New("no X-Atomic-Host header")
}

// Written before the call may happen. inv.mu is held across the RPUSH so
// list index and Effect.Seq stay equal under concurrent calls.
func (ic *interceptor) logAttempt(ctx context.Context, inv *invocation, r *http.Request, target *url.URL, body []byte) (Effect, error) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	e := Effect{
		Seq:         inv.seq,
		InvID:       inv.id,
		TS:          now(),
		Method:      r.Method,
		URL:         target.String(),
		Body:        string(body[:min(len(body), captureLimit)]),
		ContentType: r.Header.Get("Content-Type"),
		State:       "attempted",
	}
	// Drained under the same lock that assigns Seq, so no write can slip in between.
	pending := inv.pending
	inv.pending = nil
	if err := ic.store.AppendAttempted(ctx, pending, e); err != nil {
		inv.pending = pending // still owed; the next round trip carries them
		return e, err
	}
	inv.seq++
	return e, nil
}

func stripHopHeaders(h http.Header) {
	for _, k := range []string{"X-Atomic-Host", "X-Atomic-Scheme", "Proxy-Connection", "Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}

// Hop-by-hop headers must not leak onto the loopback link (an upstream "Connection:
// close" would kill the shim's keep-alive); Content-Length is wrong once Go decompresses.
func copyRespHeaders(dst http.Header, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	stripHopHeaders(dst)
	dst.Del("Content-Length")
}

func (ic *interceptor) forward(w http.ResponseWriter, r *http.Request, target *url.URL, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return 0, nil, err
	}
	req.Header = r.Header.Clone()
	stripHopHeaders(req.Header)
	// Drop Accept-Encoding so Go decompresses and the effect is recorded decompressed.
	req.Header.Del("Accept-Encoding")
	resp, err := ic.egress.Do(req)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	copyRespHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
	return resp.StatusCode, respBody, nil
}

func (ic *interceptor) outboundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, err := resolveTarget(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > maxBodyBytes {
			http.Error(w, "request body exceeds 1 MiB", http.StatusRequestEntityTooLarge)
			return
		}

		inv := ic.peek()
		if inv == nil {
			ic.forward(w, r, target, body)
			return
		}
		if ic.isDeferrable(r.Method, target) {
			ic.deferCall(w, r, inv, target, body)
			return
		}
		ic.forwardLogged(w, r, inv, target, body)
	})
}

func (ic *interceptor) forwardLogged(w http.ResponseWriter, r *http.Request, inv *invocation, target *url.URL, body []byte) {
	// Write-ahead: an effect that cannot be recorded must not happen.
	e, err := ic.logAttempt(r.Context(), inv, r, target, body)
	if err != nil {
		log.Printf("effect %s/%d: cannot write-ahead, refusing call: %v", inv.id, e.Seq, err)
		http.Error(w, "effect log unavailable", http.StatusServiceUnavailable)
		return
	}
	status, respBody, err := ic.forward(w, r, target, body)
	if err != nil {
		return // record stays "attempted"; the reverter undoes it as if it had landed
	}

	e.RespStatus = status
	e.RespBody = string(respBody[:min(len(respBody), captureLimit)])
	e.State = "committed"
	// Queued, not written: nothing reads the completion before the invocation ends.
	if w, err := completionWrite(e); err != nil {
		log.Printf("effect %s/%d: cannot encode completion: %v", inv.id, e.Seq, err)
	} else {
		inv.queue(w)
	}
}
