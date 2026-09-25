package main

// Tier-0 effects are irreversible calls like emails. They are held back and
// flushed after the function returns; the commit is gated on their delivery.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"atomicfaas/pathmatch"
)

type deferredCall struct {
	Seq         int    `json:"seq"`
	InvID       string `json:"invId"`
	TS          string `json:"ts"`
	Method      string `json:"method"`
	URL         string `json:"url"`
	Body        string `json:"body"`
	ContentType string `json:"contentType"`
}

type tier0Rule struct{ method, path string }

func loadTier0Rules(path string) []tier0Rule {
	var rules []tier0Rule
	buf, err := os.ReadFile(path)
	if err != nil {
		log.Printf("no catalog at %s, tier-0 deferral disabled: %v", path, err)
		return rules
	}
	var entries []struct {
		Match struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"match"`
		Tier *int `json:"tier"`
	}
	if err := json.Unmarshal(buf, &entries); err != nil {
		log.Printf("catalog %s unreadable, tier-0 deferral disabled: %v", path, err)
		return rules
	}
	for _, e := range entries {
		if e.Tier != nil && *e.Tier == 0 {
			rules = append(rules, tier0Rule{e.Match.Method, e.Match.Path})
		}
	}
	return rules
}

func (ic *interceptor) isDeferrable(method string, target *url.URL) bool {
	for _, r := range ic.tier0 {
		if r.method == method && pathmatch.Match(r.path, target.Path) {
			return true
		}
	}
	return false
}

func (ic *interceptor) deferCall(w http.ResponseWriter, r *http.Request, inv *invocation, target *url.URL, body []byte) {
	inv.mu.Lock()
	call := deferredCall{
		Seq:         inv.deferredSeq,
		InvID:       inv.id,
		TS:          now(),
		Method:      r.Method,
		URL:         target.String(),
		Body:        string(body),
		ContentType: r.Header.Get("Content-Type"),
	}
	// Queued: nobody reads it back before FlushPending makes it durable, ahead of the send.
	pw, err := deferredWrite(call)
	if err == nil {
		inv.pending = append(inv.pending, pw)
		inv.deferred = append(inv.deferred, call)
		inv.deferredSeq++
	}
	inv.mu.Unlock()
	if err != nil {
		log.Printf("deferred %s/d%d: cannot record, refusing call: %v", inv.id, call.Seq, err)
		http.Error(w, "effect log unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("%s:d%d", inv.id, call.Seq), "deferred": true})
}

// Success gates the commit. On failure, anything already delivered goes to report:{invId}.
func (ic *interceptor) flushDeferred(inv *invocation) error {
	inv.mu.Lock()
	calls := append([]deferredCall(nil), inv.deferred...)
	inv.mu.Unlock()
	if len(calls) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Must be durable before the calls go out, so a crash mid-send leaves the reverter a record.
	if err := ic.store.FlushPending(ctx, inv.takePending()); err != nil {
		return err
	}
	for i, c := range calls {
		if err := ic.sendDeferred(ctx, c); err != nil {
			for _, sent := range calls[:i] {
				ic.store.ReportSent(ctx, sent)
			}
			return fmt.Errorf("%s/d%d: %w", c.InvID, c.Seq, err)
		}
	}
	// deferred:{invId} is dropped by MarkCommitted, in the same transaction.
	return nil
}

// Transport errors and 5xx are retried, a 4xx is final. A retry after a lost
// response delivers twice; the call is irreversible either way.
func (ic *interceptor) sendDeferred(ctx context.Context, c deferredCall) error {
	var lastErr error
	for attempt, backoff := 0, 250*time.Millisecond; attempt < 3; attempt, backoff = attempt+1, backoff*2 {
		if attempt > 0 {
			time.Sleep(backoff)
		}
		req, err := http.NewRequestWithContext(ctx, c.Method, c.URL, strings.NewReader(c.Body))
		if err != nil {
			return err
		}
		if c.ContentType != "" {
			req.Header.Set("Content-Type", c.ContentType)
		}
		resp, err := ic.egress.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("%s %s -> %d", c.Method, c.URL, resp.StatusCode)
		if resp.StatusCode < 500 {
			break
		}
	}
	return lastErr
}
