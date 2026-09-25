// The load generator: the load driver and the mock provider APIs it drives, in one process.
// Usage: loadgen -e2 (correctness) | -e3 (performance) | -e4 (throughput).
// The mock runs OUTSIDE Kubernetes: it kills instances with the host's kubeconfig, no RBAC.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Overridden by -variants: parallel campaigns run one process per cluster.
var variants = []string{"atomic", "saga", "vanilla"}

type loadgen struct {
	mock   *mock
	client *http.Client

	n           int
	killN       int
	warmup      int
	clusterPort int
	rates       string
	duration    time.Duration
	cut         time.Duration
	cutTail     time.Duration
	pause       time.Duration
	failEvery   int
	stopBelow   float64
	stopGroup   []string
	stopDir     string
	delay       time.Duration // uniform latency on every call, set and cancel alike
	clusterHost string
	campaign    string
	seed        int
	quietFor    time.Duration
	settleFloor time.Duration
	settleMax   time.Duration
}

func main() {
	var (
		e2      = flag.Bool("e2", false, "run the correctness experiment")
		e3      = flag.Bool("e3", false, "run the performance experiment")
		e4      = flag.Bool("e4", false, "run the throughput experiment")
		rates   = flag.String("rates", "50,100,150,200,250,300", "e4: offered arrival rates (req/s)")
		vars    = flag.String("variants", "atomic,saga,vanilla", "variants this process drives")
		dur     = flag.Duration("duration", 12*time.Minute, "e4: time per cell at full rate")
		cut     = flag.Duration("cut", time.Minute, "e4: leading slice of each cell (warm-up), flagged and excluded")
		cutTail = flag.Duration("cut-tail", 0, "e4: trailing slice, flagged and excluded (0: same as -cut)")
		pause   = flag.Duration("pause", 30*time.Second, "e4: idle time between cells")
		failN   = flag.Int("fail-every", 0, "e4: every Nth payment declines (0 = failure-free)")
		stopAt  = flag.Float64("stop-below", 0, "e4: end the sweep once every variant in -stop-group has had a cell under this fraction of its offered rate (0 = run every rate)")
		stopGrp = flag.String("stop-group", "", "e4: variants, possibly driven by other processes, that must all have failed before any of them stops (default: this process's own)")
		stopDir = flag.String("stop-dir", "", "e4: directory the processes of a -stop-group share their failure markers in (default: <out>/.stop)")
		n       = flag.Int("n", 0, "runs per cell (default 50 for e2, 300 for e3)")
		nKill   = flag.Int("n-kill", 0, "runs per e2 kill cell (default: min(20, -n))")
		warmup  = flag.Int("warmup", 10, "e3: leading rounds recorded but flagged, to heat the instances")
		delayMs = flag.Int("delay-ms", 100, "latency of every mock API call")
		host    = flag.String("cluster", envOr("CLUSTER_HOST", "127.0.0.1"), "cluster ingress address")
		port    = flag.Int("port", 8090, "port the mock APIs listen on")
		cport   = flag.Int("cluster-port", 80, "cluster ingress port (31080 on a bare kubeadm/Kourier NodePort cluster)")
		seed    = flag.Int("seed", 1, "trial-order seed (e3)")
		floor   = flag.Duration("settle-floor", 5*time.Second, "how long to wait for a rollback that has not started yet")
		quiet   = flag.Duration("quiet", 750*time.Millisecond, "mock idle time that counts as settled")
		max     = flag.Duration("settle-max", 90*time.Second, "give up waiting for a rollback")
		outDir  = flag.String("out", "experiments/results/campaigns", "campaign directory root")
	)
	flag.Parse()
	variants = nil
	for _, v := range strings.Split(*vars, ",") {
		v = strings.TrimSpace(v)
		switch v {
		case "atomic", "saga", "vanilla":
			variants = append(variants, v)
		default:
			fmt.Fprintf(os.Stderr, "unknown variant %q\n", v)
			os.Exit(2)
		}
	}

	// E4 holds hundreds of connections; the default 1024-fd limit would surface as dial failures.
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err == nil && rl.Cur < rl.Max {
		rl.Cur = rl.Max
		syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rl)
	}

	picked := 0
	for _, b := range []bool{*e2, *e3, *e4} {
		if b {
			picked++
		}
	}
	if picked != 1 {
		fmt.Fprintln(os.Stderr, "pick exactly one of -e2, -e3, -e4")
		os.Exit(2)
	}

	// Every route's name resolves to the cluster address; dialling it directly keeps name
	// lookups off the timed path (the name has no AAAA record, and that answer is not cached).
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	httpClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if _, p, err := net.SplitHostPort(addr); err == nil && !strings.Contains(addr, "127.0.0.1") {
			addr = net.JoinHostPort(*host, p)
		}
		return dialer.DialContext(ctx, network, addr)
	}

	h := &loadgen{
		mock: newMock(), client: httpClient,
		n: *n, delay: time.Duration(*delayMs) * time.Millisecond,
		clusterHost: *host, clusterPort: *cport, seed: *seed, settleFloor: *floor, warmup: *warmup,
		quietFor: *quiet, settleMax: *max,
		rates: *rates, duration: *dur, cut: *cut, cutTail: *cutTail, pause: *pause, failEvery: *failN, stopBelow: *stopAt, stopDir: *stopDir,
	}
	if h.n == 0 {
		h.n = map[bool]int{true: 50, false: 300}[*e2]
	}
	if h.cutTail == 0 {
		h.cutTail = h.cut
	}
	for _, v := range strings.Split(*stopGrp, ",") {
		if v = strings.TrimSpace(v); v != "" {
			h.stopGroup = append(h.stopGroup, v)
		}
	}
	if h.stopDir == "" {
		h.stopDir = filepath.Join(*outDir, ".stop")
	}
	// A kill cell yields a leak count, not a distribution, so it needs fewer runs.
	h.killN = *nKill
	if h.killN == 0 {
		if h.killN = 20; h.n < h.killN {
			h.killN = h.n
		}
	}

	kind := "e3"
	switch {
	case *e2:
		kind = "e2"
	case *e4:
		kind = "e4"
	}
	h.campaign = filepath.Join(*outDir, kind+"-"+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(h.campaign, 0o755); err != nil {
		log.Fatal(err)
	}

	srv := &http.Server{Addr: fmt.Sprintf(":%d", *port), Handler: h.mock.routes()}
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("mock: %v", err)
		}
	}()
	if err := waitPort(*port); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("campaign=%s  n=%d  cluster=%s  mock=:%d\n",
		h.campaign, h.n, h.clusterHost, *port)
	if *e2 {
		fmt.Printf("  kill cells: n=%d\n", h.killN)
	}

	var err error
	switch {
	case *e2:
		err = h.runE2()
	case *e4:
		err = h.runE4()
	default:
		err = h.runE3()
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("campaign complete: %s\n", h.campaign)
	fmt.Printf("figure: .venv/bin/python experiments/plots/plot-%s.py %s\n", kind, h.campaign)
}

func (h *loadgen) meta(extra map[string]any) []byte {
	m := map[string]any{
		"meta":              true,
		"ts_utc":            time.Now().UTC().Format(time.RFC3339),
		"delay_ms":          h.delay.Milliseconds(),
		"cancel_delay_ms":   h.delay.Milliseconds(),
		"requests_per_cell": h.n,
		"quiet_ms":          h.quietFor.Milliseconds(),
		"settle_floor_ms":   h.settleFloor.Milliseconds(),
	}
	for k, v := range extra {
		m[k] = v
	}
	buf, _ := json.Marshal(m)
	return buf
}

func waitPort(port int) error {
	for i := 0; i < 40; i++ {
		resp, err := httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d/debug/state", port))
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("mock never came up on :%d", port)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
