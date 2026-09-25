package main

// One invocation seen from both ends: the driver times it, the mock records effects and undos.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type Run struct {
	Variant string `json:"variant"`
	Index   int    `json:"run"`
	Mode    string `json:"mode,omitempty"`    // e3: none | error
	Failure string `json:"failure,omitempty"` // e2: error | kill
	API     string `json:"api,omitempty"`     // which call fails
	Warmup  bool   `json:"warmup,omitempty"`  // ran to heat the instance; excluded from the figures

	// the client's view
	Status  int     `json:"status"`
	Latency float64 `json:"latency_ms"`

	// the mock's view
	Effects []resource `json:"effects"`
	Undos   []resource `json:"undos"`
	Events  []event    `json:"events"`

	SettledMs float64 `json:"settled_ms"` // how long the rollback was watched
	Derived   derived `json:"derived"`
}

type derived struct {
	Depth          int      `json:"depth"`            // effects that landed at all
	DepthAtFailure int      `json:"depth_at_failure"` // ...before the failure
	Leaked         int      `json:"leaked"`           // still live, and not committed
	LeakedMail     int      `json:"leaked_mail"`      // the irreversible one
	Committed      bool     `json:"committed"`
	UndoStartMs    *float64 `json:"undo_start_ms,omitempty"` // failure -> FIRST undo
	UndoMs         *float64 `json:"undo_ms,omitempty"`       // failure -> last undo
}

// failedAt is stamped by the mock, like every timestamp it is compared against.
func (r *Run) failedAt() *time.Time {
	for i := range r.Events {
		if e := r.Events[i]; e.Kind == "rejected" || e.Kind == "hung" {
			return &e.At
		}
	}
	return nil
}

func (r *Run) hung() bool {
	for _, e := range r.Events {
		if e.Kind == "hung" {
			return true
		}
	}
	return false
}

// Nothing can take back a mail, so it is counted apart from the bookings.
func isMail(x resource) bool { return x.Store == "emails" }

func (r *Run) derive() {
	r.Derived.Committed = r.Status == 200
	fail := r.failedAt()
	for _, e := range r.Effects {
		if isMail(e) {
			continue
		}
		r.Derived.Depth++
		// Only effects that existed at the failure count toward the depth.
		if fail == nil || e.CreatedAt.Before(*fail) {
			r.Derived.DepthAtFailure++
		}
	}
	if fail == nil {
		r.Derived.DepthAtFailure = r.Derived.Depth
	}

	// Live after a non-200 is a leak; live after a 200 is the trip.
	live := map[string]bool{}
	for _, e := range r.Effects {
		live[e.ID] = !isMail(e)
	}
	for _, u := range r.Undos {
		live[u.ID] = false
	}
	for _, e := range r.Effects {
		if live[e.ID] && !r.Derived.Committed {
			r.Derived.Leaked++
		}
		if isMail(e) && !r.Derived.Committed {
			r.Derived.LeakedMail++
		}
	}

	if fail != nil && len(r.Undos) > 0 {
		var first, last *time.Time
		for _, u := range r.Undos {
			if u.StatusAt == nil {
				continue
			}
			if first == nil || u.StatusAt.Before(*first) {
				first = u.StatusAt
			}
			if last == nil || u.StatusAt.After(*last) {
				last = u.StatusAt
			}
		}
		if first != nil {
			ms := float64(first.Sub(*fail).Microseconds()) / 1000
			r.Derived.UndoStartMs = &ms
		}
		if last != nil {
			ms := float64(last.Sub(*fail).Microseconds()) / 1000
			r.Derived.UndoMs = &ms
		}
	}
}

func (h *loadgen) invoke(run *Run) {
	h.mock.begin(run)
	start := time.Now()
	resp, err := h.client.Get(h.route(run.Variant) + "/")
	run.Latency = float64(time.Since(start).Microseconds()) / 1000
	if err == nil {
		run.Status = resp.StatusCode
		// Drained so the connection is reused; an unread body makes Go open a new one per
		// trial, and a fresh connection after seconds of idleness costs ~30 ms now and then.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	} // err leaves Status 0: a transport failure, kept as such

	// Successful runs settle too: the deferred mail flushes after the client is answered
	// and would otherwise land in the next run. No floor: it is already under way.
	settleStart := time.Now()
	if run.Status == 200 {
		h.settle(0)
	} else {
		h.settle(h.settleFloor)
	}
	run.SettledMs = float64(time.Since(settleStart).Microseconds()) / 1000

	h.mock.end()
	run.derive()

	// A real leak and a too-short floor look identical in the data.
	if run.Variant == "atomic" && run.Derived.Leaked > 0 {
		fmt.Fprintf(os.Stderr,
			"  warning: atomic left %d effect(s) live (run %d, %s@%s). If that is "+
				"not expected, -settle-floor (%s) may be too short for this platform.\n",
			run.Derived.Leaked, run.Index, run.Failure, run.API, h.settleFloor)
	}
}

// settle waits for the mock to go quiet. An idle mock may also mean the rollback has
// not begun yet, so floor is waited out unless an undo has already arrived.
func (h *loadgen) settle(floor time.Duration) {
	deadline := time.Now().Add(h.settleMax)
	begun := time.Now()
	for time.Now().Before(deadline) {
		if h.mock.quietFor() >= h.quietFor &&
			(h.mock.undoCount() > 0 || time.Since(begun) >= floor) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "  warning: mock still busy after %s\n", h.settleMax)
}

// waitRoute wants two healthz in a row: a pod on its way out serves one and drops the next.
func (h *loadgen) waitRoute(variant string) error {
	ok := 0
	for i := 0; i < 240; i++ {
		resp, err := h.client.Get(h.route(variant) + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				if ok++; ok >= 2 {
					return nil
				}
			} else {
				ok = 0
			}
		} else {
			ok = 0
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("route for %s never answered at %s - wrong -cluster/CLUSTER_HOST, or the cluster is down",
		variant, h.route(variant))
}

// prepare creates every cell file up front, opening each with the meta record so it stands alone.
func (h *loadgen) prepare(names []string, meta []byte) error {
	for _, n := range names {
		f, err := os.OpenFile(filepath.Join(h.campaign, n), os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		f.Write(append(meta, '\n'))
		f.Close()
	}
	return nil
}

// record reopens the file per run so the line is on disk when the run ends.
func (h *loadgen) record(path string, run any) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(run)
}

func (h *loadgen) route(variant string) string {
	port := ""
	if h.clusterPort != 80 {
		port = fmt.Sprintf(":%d", h.clusterPort)
	}
	return fmt.Sprintf("http://%s.default.%s.sslip.io%s", serviceFor(variant), h.clusterHost, port)
}

func serviceFor(variant string) string {
	switch variant {
	case "atomic":
		return "atomic-extended"
	case "saga":
		return "saga-extended"
	default:
		return "vanilla-extended"
	}
}

// E4 holds hundreds of connections to one host; the default keeps only 2 idle per host.
var httpClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: 1024,
	},
}
