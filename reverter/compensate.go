package main

// The compensation walk: undoes effects in reverse order; a record that
// cannot be undone is floored (reported) and the walk continues.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const maxBodyBytes = 1 << 20

type reverter struct {
	store   *store
	catalog catalog
	client  *http.Client
	workers int

	// An invocation stays in deadlines until MarkDone; without this guard the
	// next poll would hand it out again.
	inflight sync.Map
}

// Walks of different invocations run in parallel. Within one invocation the
// LIFO undo order is a semantic, so it stays sequential.
func (rv *reverter) run(interval time.Duration) {
	slots := make(chan struct{}, rv.workers)
	for range time.Tick(interval) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		due, err := rv.store.Due(ctx)
		cancel()
		if err != nil {
			log.Printf("deadlines poll: %v", err)
			continue
		}
		for _, invID := range due {
			if _, busy := rv.inflight.LoadOrStore(invID, struct{}{}); busy {
				continue
			}
			slots <- struct{}{}
			go func(invID string) {
				defer func() { <-slots; rv.inflight.Delete(invID) }()
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				rv.compensate(ctx, invID)
				cancel()
			}(invID)
		}
	}
}

func (rv *reverter) compensate(ctx context.Context, invID string) {
	if err := rv.store.MarkCompensating(ctx, invID); err != nil {
		log.Printf("%s: cannot mark compensating: %v (will retry next tick)", invID, err)
		return
	}
	effects, err := rv.store.LoadLog(ctx, invID)
	if err != nil {
		log.Printf("%s: cannot load log: %v (will retry next tick)", invID, err)
		return
	}
	status := "compensated"
	for i := len(effects) - 1; i >= 0; i-- {
		e := effects[i]
		if err := rv.undo(ctx, e); err != nil {
			rv.floor(ctx, e, err.Error())
			status = "floored"
		}
	}
	if err := rv.store.ReportDeferred(ctx, invID); err != nil {
		log.Printf("%s: cannot report deferred calls: %v", invID, err)
	}
	if err := rv.store.MarkDone(ctx, invID, status); err != nil {
		log.Printf("%s: cannot mark done: %v (will retry next tick)", invID, err)
		return
	}
	log.Printf("invocation %s: %s (%d effects)", invID, status, len(effects))
}

func (rv *reverter) undo(ctx context.Context, e Effect) error {
	r := rv.catalog.match(e.Method, e.URL)
	if effectiveTier(r) != 1 {
		return errors.New("no compensation cataloged")
	}

	// A refused call left nothing behind.
	if e.State == "committed" && (e.RespStatus < 200 || e.RespStatus >= 300) {
		return nil
	}
	// An "attempted" record has no response: the call was in flight at the crash and
	// may or may not have landed. The inverse is issued if its template renders from
	// the request alone; one that needs the response is floored. A 404 counts as done.
	path, err := renderString(r.Inverse.Path, e, e.RespBody)
	if err != nil {
		return fmt.Errorf("inverse template: %w", err)
	}
	invBody, err := renderBody(r.Inverse.Body, e, e.RespBody)
	if err != nil {
		return fmt.Errorf("inverse template: %w", err)
	}
	// The inverse goes to the same origin the function called.
	u, err := url.Parse(e.URL)
	if err != nil {
		return err
	}
	status, _, err := rv.call(ctx, r.Inverse.Method, u.Scheme+"://"+u.Host+path, invBody, "")
	if err != nil || !inverseSucceeded(status) {
		return fmt.Errorf("inverse %s %s failed: status=%d err=%v", r.Inverse.Method, path, status, err)
	}
	return nil
}

// An undo aims for a state, not an action: 404 means already gone.
func inverseSucceeded(status int) bool {
	return (status >= 200 && status < 300) || status == http.StatusNotFound
}

// Retries transport errors and 5xx; an inverse aims for a state, so repeating it is safe.
func (rv *reverter) call(ctx context.Context, method, url string, body []byte, contentType string) (int, string, error) {
	if contentType == "" {
		contentType = "application/json"
	}
	var lastErr error
	for attempt, backoff := 0, 250*time.Millisecond; attempt < 3; attempt, backoff = attempt+1, backoff*2 {
		if attempt > 0 {
			time.Sleep(backoff)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return 0, "", err
		}
		if len(body) > 0 {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := rv.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s %s -> %d", method, url, resp.StatusCode)
			continue
		}
		return resp.StatusCode, string(body), nil
	}
	return 0, "", lastErr
}

func (rv *reverter) floor(ctx context.Context, e Effect, reason string) {
	log.Printf("%s/%d: FLOORED: %s", e.InvID, e.Seq, reason)
	rv.store.Report(ctx, e.InvID, map[string]any{
		"seq": e.Seq, "method": e.Method, "url": e.URL,
		"state": e.State, "reason": reason, "ts": now(),
	})
}
