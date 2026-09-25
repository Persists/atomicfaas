package main

// E2 - correctness: each matrix row fails one API (error: it refuses; kill: it hangs
// and the instance is killed there), sweeping depth 0-5 by construction.
// Killed instances restart in place (bootstrap.sh caps CrashLoopBackOff); nothing is deleted.

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"time"
)

var e2APIs = []string{"hotel", "car", "flight", "payment", "mail", "trip"}

var storeOf = map[string]string{
	"hotel": "hotels", "car": "cars", "flight": "flights",
	"payment": "payments", "mail": "emails", "trip": "trips",
}

// Each API refuses the way its real provider documents, not via injected exceptions.
func (h *loadgen) errorCfg(api string) failureCfg {
	cfg := failureCfg{delay: h.delay, cancelWait: h.delay}
	switch api {
	case "hotel", "car", "flight":
		cfg.noCapacity = api
	case "payment":
		cfg.decline = true
	case "mail":
		cfg.failMail = true
	case "trip":
		cfg.failSvc = "trip"
	}
	return cfg
}

func (h *loadgen) runE2() error {
	var names []string
	for _, api := range e2APIs {
		for _, variant := range variants {
			names = append(names, fmt.Sprintf("aborts-%s-%s.jsonl", api, variant))
		}
	}
	for _, variant := range variants {
		names = append(names, fmt.Sprintf("crashes-%s.jsonl", variant))
	}
	if err := h.prepare(names, h.meta(map[string]any{
		"experiment":         "e2-correctness",
		"kill_runs_per_cell": h.killN,
	})); err != nil {
		return err
	}

	// error rows: the API refuses, the function stays alive
	for _, api := range e2APIs {
		for _, variant := range variants {
			h.mock.configure(h.errorCfg(api))
			h.mock.setCrash(nil)
			if err := h.waitRoute(variant); err != nil {
				return err
			}
			out := filepath.Join(h.campaign, fmt.Sprintf("aborts-%s-%s.jsonl", api, variant))
			fmt.Printf("=== error@%s / %s (n=%d)\n", api, variant, h.n)
			for i := 1; i <= h.n; i++ {
				run := h.errorRun(variant, api, i)
				if err := h.record(out, run); err != nil {
					return err
				}
			}
		}
	}

	// kill rows: the API hangs and the instance is killed
	for _, api := range e2APIs {
		for _, variant := range variants {
			cfg := failureCfg{delay: h.delay, cancelWait: h.delay, hangAt: storeOf[api]}
			svc, method := serviceFor(variant), killMethod(variant)
			h.mock.configure(cfg)
			h.mock.setCrash(func() { h.kill(svc, method) })
			out := filepath.Join(h.campaign, fmt.Sprintf("crashes-%s.jsonl", variant))
			fmt.Printf("=== kill@%s / %s (n=%d)\n", api, variant, h.killN)
			for i := 1; i <= h.killN; i++ {
				if err := h.waitRoute(variant); err != nil {
					return err
				}
				run := h.killRun(variant, api, i)
				if run == nil {
					continue // never reached a live instance; already reported
				}
				if err := h.record(out, run); err != nil {
					return err
				}
			}
		}
	}
	h.mock.setCrash(nil)
	return nil
}

// errorRun retries until the mock actually refused; a transient ingress 502 would land at (0,0).
func (h *loadgen) errorRun(variant, api string, i int) *Run {
	for attempt := 1; attempt <= 3; attempt++ {
		run := &Run{Variant: variant, Index: i, Failure: "error", API: api}
		h.invoke(run)
		if run.failedAt() != nil {
			return run
		}
		fmt.Printf("    retry %d: the mock never refused (status %d)\n", attempt, run.Status)
		h.waitRoute(variant)
	}
	run := &Run{Variant: variant, Index: i, Failure: "error", API: api}
	h.invoke(run)
	return run // recorded regardless; the figures see its odd status
}

// killRun retries until the mock actually held a call; an answer that never reached a live instance killed nothing.
func (h *loadgen) killRun(variant, api string, i int) *Run {
	for attempt := 1; attempt <= 4; attempt++ {
		run := &Run{Variant: variant, Index: i, Failure: "kill", API: api}
		h.invoke(run)
		if run.hung() {
			return run
		}
		fmt.Printf("    retry %d: request never reached a live instance\n", attempt)
		h.resetPods(serviceFor(variant))
		if err := h.waitRoute(variant); err != nil {
			return nil
		}
	}
	fmt.Printf("    giving up on run %d\n", i)
	return nil
}

// Recovery only; the steady-state loop never deletes pods.
func (h *loadgen) resetPods(service string) {
	sel := "serving.knative.dev/service=" + service
	exec.Command("kubectl", "delete", "pod", "-l", sel,
		"--grace-period=1", "--wait=false").Run()
	time.Sleep(time.Second)
	exec.Command("kubectl", "wait", "pod", "-l", sel,
		"--for=condition=Ready", "--timeout=120s").Run()
}
