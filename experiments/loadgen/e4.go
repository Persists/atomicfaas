package main

// E4 - throughput. Open-loop: each cell offers a constant arrival rate on a fixed clock
// regardless of in-flight requests, so latency includes queueing and the saturation knee
// shows (no coordinated omission). Runs are concurrent, so each record is the client's view and
// the mock's counts are closed per cell.

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type e4Rec struct {
	Variant  string  `json:"variant"`
	Rate     int     `json:"rate"` // offered arrivals per second
	Status   int     `json:"status"`
	Latency  float64 `json:"latency_ms"`
	StartMs  float64 `json:"start_ms"`           // offset into the cell
	Warmup   bool    `json:"warmup,omitempty"`   // issued during the ramp
	Cooldown bool    `json:"cooldown,omitempty"` // issued in the trailing slice
	Body     string  `json:"body,omitempty"`     // head of a non-200 response: who rejected, and why
}

func (h *loadgen) runE4() error {
	rates, err := parseLevels(h.rates)
	if err != nil {
		return err
	}

	var names []string
	for _, r := range rates {
		for _, v := range variants {
			names = append(names, fmt.Sprintf("e4-r%03d-%s.jsonl", r, v))
		}
	}
	if err := h.prepare(names, h.meta(map[string]any{
		"experiment":   "e4-throughput",
		"cluster_host": h.clusterHost,
		"offered_rps":  rates,
		"duration_s":   h.duration.Seconds(),
		"cut_s":        h.cut.Seconds(),
		"cut_tail_s":   h.cutTail.Seconds(),
		"pause_s":      h.pause.Seconds(),
		"fail_every_n": h.failEvery,
		"stop_below":   h.stopBelow,
		"order_seed":   h.seed,
	})); err != nil {
		return err
	}

	// Loud: the flag default (100ms) suits E2/E3 but is wrong for pure throughput.
	fmt.Printf("mock call delay: %s (pass -delay-ms 0 for pure platform throughput)\n", h.delay)
	if h.failEvery > 0 {
		fmt.Printf("every %dth payment declines\n", h.failEvery)
	}
	h.mock.configure(failureCfg{delay: h.delay, cancelWait: h.delay, declineEveryN: h.failEvery})
	h.mock.setCrash(nil)

	rng := rand.New(rand.NewSource(int64(h.seed)))
	for _, r := range rates {
		order := append([]string(nil), variants...)
		rng.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })
		for _, v := range order {
			if err := h.waitRoute(v); err != nil {
				return err
			}
			// A fresh mock per cell, so the cell's resources are exactly what it created.
			h.mock.configure(failureCfg{delay: h.delay, cancelWait: h.delay, declineEveryN: h.failEvery})
			start, declined, achieved, err := h.e4Cell(v, r)
			if err != nil {
				return err
			}
			// Drain deferred mails, stragglers and out-of-band compensations, then account.
			h.settle(0)
			if err := h.e4Leaks(v, r, start, declined); err != nil {
				return err
			}
			// Past its ceiling every higher rate fails too (the plots drop such cells), but a
			// variant keeps offering load beside the others until the whole group has folded.
			want := float64(r)
			if h.failEvery > 0 {
				want *= 1 - 1/float64(h.failEvery)
			}
			if h.stopBelow > 0 && achieved < h.stopBelow*want {
				fmt.Printf("    %s: %.1f req/s is under %.0f%% of the expected %.0f; at its ceiling\n",
					v, achieved, 100*h.stopBelow, want)
				if err := h.markFailed(v, r); err != nil {
					return err
				}
			}
			if h.stopBelow > 0 && h.groupFailed() {
				fmt.Printf("    every variant of the group has reached its ceiling; ending the sweep\n")
				return nil
			}
			time.Sleep(h.pause)
		}
	}
	return nil
}

func (h *loadgen) e4Cell(variant string, rate int) (time.Time, int, float64, error) {
	out := filepath.Join(h.campaign, fmt.Sprintf("e4-r%03d-%s.jsonl", rate, variant))
	fmt.Printf("=== %d req/s / %s (%s)\n", rate, variant, h.duration)

	var mu sync.Mutex
	var recs []e4Rec
	url := h.route(variant) + "/"
	start := time.Now()
	deadline := start.Add(h.duration)

	var wg sync.WaitGroup
	fire := func(t0 time.Time, warm, cool bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body := 0, ""
			if resp, err := h.client.Get(url); err == nil {
				status = resp.StatusCode
				// Drain the body for keep-alive reuse; keep the head of failures, since a
				// queue-proxy 503 and an interceptor 503 are different diagnoses.
				head := make([]byte, 96)
				n, _ := io.ReadFull(resp.Body, head)
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if status != 200 {
					body = strings.TrimSpace(string(head[:n]))
				}
			} else {
				body = err.Error()
			}
			rec := e4Rec{
				Variant: variant, Rate: rate, Status: status,
				Latency: float64(time.Since(t0).Microseconds()) / 1000,
				StartMs: float64(t0.Sub(start).Microseconds()) / 1000,
				Warmup:  warm, Cooldown: cool, Body: body,
			}
			mu.Lock()
			recs = append(recs, rec)
			mu.Unlock()
		}()
	}

	warmUntil := start.Add(h.cut)
	coolFrom := deadline.Add(-h.cutTail)
	tick := time.NewTicker(time.Second / time.Duration(rate))
	defer tick.Stop()
	for t0 := start; t0.Before(deadline); t0 = <-tick.C {
		fire(t0, t0.Before(warmUntil), !t0.Before(coolFrom))
	}
	wg.Wait()

	// Tickers drop ticks rather than catch up; a starved driver would silently under-offer.
	expected := int(h.duration.Seconds()) * rate
	if len(recs) < expected*99/100 {
		fmt.Printf("    WARNING: issued %d of %d intended requests - the "+
			"load generator itself fell behind; this cell under-offers\n",
			len(recs), expected)
	}

	ok, declined := 0, 0
	for _, r := range recs {
		if !r.Warmup && !r.Cooldown && r.Status == 200 {
			ok++
		}
		if r.Status == http.StatusPaymentRequired {
			declined++
		}
	}
	achieved := float64(ok) / (h.duration - h.cut - h.cutTail).Seconds()
	fmt.Printf("    offered %d req/s (issued %d/%d), achieved %.1f req/s\n",
		rate, len(recs), expected, achieved)
	for _, r := range recs {
		if err := h.record(out, &r); err != nil {
			return start, declined, achieved, err
		}
	}
	return start, declined, achieved, nil
}

// e4Leaks closes the cell's books against the mock: a declined trip leaves
// three reservations that must be canceled by somebody, so per store
// declined - canceled is the leak. With declines on, the snapshot keeps every
// resource with its timestamps; without, it would be millions of identical lines per cell.
func (h *loadgen) e4Leaks(variant string, rate int, start time.Time, declined int) error {
	created, canceled := map[string]int{}, map[string]int{}
	snap := h.mock.snapshot()
	for _, r := range snap {
		created[r.Store]++
		if r.Status == "canceled" {
			canceled[r.Store]++
		}
	}
	if declined > 0 {
		fmt.Printf("    declined %d; canceled hotels=%d flights=%d cars=%d\n",
			declined, canceled["hotels"], canceled["flights"], canceled["cars"])
	}
	out := filepath.Join(h.campaign, fmt.Sprintf("e4-r%03d-%s.jsonl", rate, variant))
	if err := h.record(out, map[string]any{
		"summary": true, "cell_start_utc": start.UTC().Format(time.RFC3339Nano),
		"declined": declined, "created": created, "canceled": canceled,
	}); err != nil {
		return err
	}
	if h.failEvery == 0 {
		return nil
	}
	side := filepath.Join(h.campaign, fmt.Sprintf("e4-r%03d-%s-mock.jsonl", rate, variant))
	for _, r := range snap {
		if err := h.record(side, r); err != nil {
			return err
		}
	}
	return nil
}

// Failure markers are files, so the processes driving the other variants see them.
func (h *loadgen) markFailed(variant string, rate int) error {
	if err := os.MkdirAll(h.stopDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(h.stopDir, variant), []byte(strconv.Itoa(rate)+"\n"), 0o644)
}

func (h *loadgen) groupFailed() bool {
	group := h.stopGroup
	if len(group) == 0 {
		group = variants
	}
	for _, v := range group {
		if _, err := os.Stat(filepath.Join(h.stopDir, v)); err != nil {
			return false
		}
	}
	return true
}

func parseLevels(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("bad rate list %q", s)
		}
		out = append(out, n)
	}
	return out, nil
}
