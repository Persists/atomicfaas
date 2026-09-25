package main

// The mock provider APIs the trip-booking workload calls: hotel/car/flight/trip
// reservations with cancel, a Stripe-modeled payment, and a SendGrid-modeled
// mail send with no inverse. E2 and E3 attribute records to
// one run at a time; E4 only reads the per-cell counts.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Inverses flip Status rather than deleting, so the history stays visible.
type resource struct {
	ID        string     `json:"id"`
	Store     string     `json:"store"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"createdAt"`
	StatusAt  *time.Time `json:"statusAt,omitempty"`
	// When the inverse request arrived; StatusAt is when it completed.
	UndoAt *time.Time `json:"undoAt,omitempty"`
}

// An event that changed nothing but matters: a refusal creates no resource; a hang pins the function mid-call.
type event struct {
	Kind string    `json:"kind"` // rejected | hung
	API  string    `json:"api"`
	At   time.Time `json:"at"`
}

type storeSpec struct{ prefix, created string }

var stores = map[string]storeSpec{
	"hotels":   {"htl", "reserved"},
	"cars":     {"car", "reserved"},
	"flights":  {"flt", "reserved"},
	"payments": {"pi", "created"},
	"trips":    {"trip", "confirmed"},
	"emails":   {"em", "sent"},
}

type mock struct {
	mu    sync.Mutex
	seq   int
	items map[string]*resource // id -> resource, across all stores

	cfg      failureCfg
	payments int       // fresh payment attempts this cell, for declineEveryN
	run      *Run      // the invocation in flight; nil between runs
	last     time.Time // last time anything happened, for settling

	// Under the lock: a stale read would fire the previous cell's kill.
	crash func()
}

// Set once per cell, never per request.
type failureCfg struct {
	delay         time.Duration // every forward call
	cancelWait    time.Duration // every inverse
	noCapacity    string        // hotel|car|flight answers 409
	decline       bool          // payment answers 402
	declineEveryN int           // every Nth fresh payment answers 402 (throughput under failure)
	failSvc       string        // hotel|car|flight|trip answers 500
	failMail      bool          // mail answers 500
	hangAt        string        // store name whose API stops answering
}

func newMock() *mock {
	return &mock{
		items: map[string]*resource{},
		last:  time.Now(),
	}
}

func (m *mock) begin(r *Run) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.run, m.last = r, time.Now()
}

func (m *mock) end() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.run = nil
}

func (m *mock) quietFor() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return time.Since(m.last)
}

func (m *mock) undoCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.run == nil {
		return 0
	}
	return len(m.run.Undos)
}

func (m *mock) setCrash(f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.crash = f
}

func (m *mock) crashFn() func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.crash
}

func (m *mock) configure(cfg failureCfg) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
	m.payments = 0
	m.items = map[string]*resource{}
	m.last = time.Now()
}

func (m *mock) create(store string) *resource {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	spec := stores[store]
	r := &resource{
		ID: fmt.Sprintf("%s_%04d", spec.prefix, m.seq), Store: store,
		Status: spec.created, CreatedAt: time.Now(),
	}
	m.items[r.ID] = r
	m.last = r.CreatedAt
	if m.run != nil {
		m.run.Effects = append(m.run.Effects, *r)
	}
	return r
}

func (m *mock) cancel(id string, arrived time.Time) *resource {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.items[id]
	if r == nil {
		return nil
	}
	now := time.Now()
	r.Status, r.StatusAt, r.UndoAt, m.last = "canceled", &now, &arrived, now
	if m.run != nil {
		m.run.Undos = append(m.run.Undos, *r)
	}
	return r
}

func (m *mock) note(kind, api string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := event{Kind: kind, API: api, At: time.Now()}
	m.last = e.At
	if m.run != nil {
		m.run.Events = append(m.run.Events, e)
	}
	return e.At
}

func (m *mock) snapshot() []resource {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]resource, 0, len(m.items))
	for _, r := range m.items {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// id "none" lets a platform that logged the attempt still render its inverse, which then 404s.
func (m *mock) refused(w http.ResponseWriter, code int, api, msg string) {
	m.note("rejected", api)
	writeJSON(w, code, map[string]string{"error": msg, "id": "none"})
}

func (m *mock) forward(api, store string, okCode int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := m.currentCfg()

		// Hang first: it pins the function at a known depth for the kill.
		if cfg.hangAt == store {
			m.note("hung", api)
			if c := m.crashFn(); c != nil {
				go c()
			}
			<-r.Context().Done() // held until the caller goes away
			return
		}

		if (cfg.failSvc == api) || (api == "mail" && cfg.failMail) {
			m.refused(w, http.StatusInternalServerError, api, api+" backend down")
			return
		}
		if cfg.noCapacity == api {
			m.refused(w, http.StatusConflict, api, "no capacity")
			return
		}
		if api == "payment" && cfg.decline {
			m.refused(w, http.StatusPaymentRequired, api, "card_declined")
			return
		}
		if api == "payment" && cfg.declineEveryN > 0 && m.nthPayment(cfg.declineEveryN) {
			m.refused(w, http.StatusPaymentRequired, api, "card_declined")
			return
		}

		time.Sleep(cfg.delay)
		io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
		writeJSON(w, okCode, m.create(store))
	}
}

// inverse is a documented undo call; a 404 means never there, which counts as success.
func (m *mock) inverse() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		arrived := time.Now()
		time.Sleep(m.currentCfg().cancelWait)
		if res := m.cancel(r.PathValue("id"), arrived); res != nil {
			writeJSON(w, http.StatusOK, res)
			return
		}
		http.NotFound(w, r)
	}
}

func (m *mock) nthPayment(n int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.payments++
	return m.payments%n == 0
}

func (m *mock) currentCfg() failureCfg {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

func (m *mock) routes() *http.ServeMux {
	mux := http.NewServeMux()
	for _, s := range []struct{ path, api, store string }{
		{"/hotel/reservations", "hotel", "hotels"},
		{"/car/reservations", "car", "cars"},
		{"/flight/reservations", "flight", "flights"},
		{"/trip/confirmations", "trip", "trips"},
	} {
		mux.HandleFunc("POST "+s.path, m.forward(s.api, s.store, http.StatusCreated))
		mux.HandleFunc("POST "+s.path+"/{id}/cancel", m.inverse())
	}
	mux.HandleFunc("POST /v1/payment_intents", m.forward("payment", "payments", http.StatusCreated))
	mux.HandleFunc("POST /v1/payment_intents/{id}/cancel", m.inverse())
	mux.HandleFunc("POST /v3/mail/send", m.forward("mail", "emails", http.StatusAccepted)) // no inverse exists
	mux.HandleFunc("GET /debug/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.snapshot())
	})
	return mux
}
