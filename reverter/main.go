// The reverter compensates aborted invocations: it polls the deadlines
// queue and walks each effect log in reverse.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	redisAddr := flag.String("redis", envOr("REDIS_ADDR", "localhost:6379"), "Redis address")
	catalogPath := flag.String("catalog", envOr("CATALOG", "/catalog.json"), "compensation catalog")
	interval := flag.Duration("interval", 500*time.Millisecond, "poll interval")
	workers := flag.Int("workers", 16, "concurrent compensation walks")
	flag.Parse()

	cat, err := loadCatalog(*catalogPath)
	if err != nil {
		log.Fatalf("catalog: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: *redisAddr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("redis unreachable at %s: %v", *redisAddr, err)
	}

	rv := &reverter{
		store:   &store{rdb: rdb},
		catalog: cat,
		client:  &http.Client{Timeout: 10 * time.Second},
		workers: *workers,
	}

	log.Printf("reverter: %d catalog rules, polling every %s, %d workers", len(cat), *interval, *workers)
	rv.run(*interval)
}
