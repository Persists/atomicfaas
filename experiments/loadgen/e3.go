package main

// E3 - performance: the cost of atomicity when nothing fails (none) and of the rollback
// when the last call fails (error). Trials interleave in RMIT order (Abedi & Brecht,
// ICPE'17); latency is the measurement, so this runs on a machine other than the cluster.

import (
	"fmt"
	"math/rand"
	"path/filepath"
)

func (h *loadgen) runE3() error {
	var names []string
	for _, mode := range []string{"none", "error"} {
		for _, variant := range variants {
			names = append(names, fmt.Sprintf("e3-%s-%s.jsonl", mode, variant))
		}
	}
	if err := h.prepare(names, h.meta(map[string]any{
		"experiment":    "e3-performance",
		"cluster_host":  h.clusterHost,
		"rmit_seed":     h.seed,
		"warmup_rounds": h.warmup,
	})); err != nil {
		return err
	}

	for _, mode := range []string{"none", "error"} {
		cfg := failureCfg{delay: h.delay, cancelWait: h.delay}
		if mode == "error" {
			cfg.failSvc = "trip"
		}
		h.mock.configure(cfg)
		h.mock.setCrash(nil) // E3 never kills

		for _, variant := range variants {
			if err := h.waitRoute(variant); err != nil {
				return err
			}
		}

		// Warm-up rounds are recorded like measured trials and only flagged.
		rounds := h.warmup + h.n
		order := h.trialOrder(mode, rounds)
		fmt.Printf("=== %s (n=%d per variant + %d warmup, RMIT seed=%d)\n",
			mode, h.n, h.warmup, h.seed)
		for i := 1; i <= rounds; i++ {
			for _, variant := range order[i-1] {
				run := &Run{Variant: variant, Index: i, Mode: mode, Warmup: i <= h.warmup}
				h.invoke(run)
				out := filepath.Join(h.campaign, fmt.Sprintf("e3-%s-%s.jsonl", mode, variant))
				if err := h.record(out, run); err != nil {
					return err
				}
			}
			if i%25 == 0 {
				fmt.Printf("    %s: %d/%d per variant\n", mode, i, rounds)
			}
		}
	}
	return nil
}

// trialOrder draws a fresh seeded ordering every round, reproducible from rmit_seed.
func (h *loadgen) trialOrder(mode string, rounds int) [][]string {
	// Per-mode streams derived from the name, so -seed 1 error and -seed 2 none cannot collide.
	var mix int64
	for _, c := range mode {
		mix = mix*31 + int64(c)
	}
	rng := rand.New(rand.NewSource(int64(h.seed)*1_000_003 + mix))
	out := make([][]string, rounds)
	for i := range out {
		order := append([]string(nil), variants...)
		rng.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })
		out[i] = order
	}
	return out
}
