// The interceptor wraps the unmodified function process: it write-ahead logs
// outbound effects and decides the commit. Usage: interceptor <function argv...>
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: interceptor <function argv...>")
	}
	port := envOr("PORT", "8080")
	pod := envOr("HOSTNAME", "unknown-pod")

	redisAddr := envOr("REDIS_ADDR", "redis.default.svc.cluster.local:6379")
	st := newStore(redisAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.Ping(ctx); err != nil {
		log.Fatalf("effect store unreachable at %s: %v", redisAddr, err) // fail closed
	}

	if orphans, err := st.RecoverOrphans(ctx, pod); err != nil {
		log.Printf("interceptor: orphan sweep failed: %v", err) // not fatal: the detector is the primary path
	} else if len(orphans) > 0 {
		log.Printf("interceptor: queued %d orphan(s) left by a previous container: %v", len(orphans), orphans)
	}

	ic := &interceptor{
		store:  st,
		pod:    pod,
		tier0:  loadTier0Rules(envOr("ATOMIC_CATALOG", "/atomic/catalog.json")),
		egress: &http.Client{Timeout: egressTimeout},
	}

	cmd := spawnFunction(os.Args[1:])
	go watchFunction(cmd)
	waitForFunction(functionAddr)

	go func() { log.Fatal(http.ListenAndServe(proxyAddr, ic.outboundHandler())) }()
	log.Printf("interceptor: pod=%s inbound=:%s function=%s proxy=%s", pod, port, functionAddr, proxyAddr)
	log.Fatal(http.ListenAndServe(":"+port, ic.inboundHandler()))
}
