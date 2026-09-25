package main

// The handoff turns a crashed pod into invocations on the deadlines queue.
// running:{pod} is a SET: the restarted container may pick up a new invocation
// before this runs, and a single slot would overwrite the only pointer to the dead one.

import (
	"context"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

type handoff struct {
	rdb *redis.Client
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// The running stamp is what startedAfterTermination reads.
func stamp(ctx context.Context, p redis.Pipeliner, invID, status string) {
	p.Set(ctx, "status:"+invID, status, 0)
	p.HSet(ctx, "timeline:"+invID, status, now())
}

func (h *handoff) run(ctx context.Context, podName string, terminatedAt time.Time) {
	key := "running:" + podName
	// WATCH: if the interceptor commits between the read and the write, EXEC
	// fails instead of marking a committed invocation crashed.
	err := h.rdb.Watch(ctx, func(tx *redis.Tx) error {
		invIDs, err := tx.SMembers(ctx, key).Result()
		if err != nil && err != redis.Nil {
			return err
		}
		if len(invIDs) == 0 {
			log.Printf("handoff: pod=%s had nothing in flight", podName)
			return nil
		}

		var queue []string
		for _, invID := range invIDs {
			if startedAfterTermination(ctx, tx, invID, terminatedAt) {
				log.Printf("handoff: pod=%s inv=%s started after termination, leaving it alone", podName, invID)
				continue
			}
			queue = append(queue, invID)
		}
		if len(queue) == 0 {
			return nil
		}

		score := float64(time.Now().UnixMilli()) / 1000.0
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
			for _, invID := range queue {
				stamp(ctx, p, invID, "crashed")
				p.ZAdd(ctx, "deadlines", redis.Z{Score: score, Member: invID})
				p.SRem(ctx, key, invID)
			}
			return nil
		})
		if err == nil {
			log.Printf("handoff: pod=%s inv=%v -> deadlines", podName, queue)
		}
		return err
	}, key)
	if err == redis.TxFailedErr {
		log.Printf("handoff: pod=%s commit or self-abort raced this handoff, nothing to do", podName)
	} else if err != nil {
		log.Printf("handoff: pod=%s redis error: %v", podName, err)
	}
}

// An invocation started after the termination belongs to the restarted container.
// The second of slack absorbs FinishedAt's granularity; an unreadable start hands off.
func startedAfterTermination(ctx context.Context, tx *redis.Tx, invID string, terminatedAt time.Time) bool {
	if terminatedAt.IsZero() {
		return false
	}
	startTS, err := tx.HGet(ctx, "timeline:"+invID, "running").Result()
	if err != nil {
		return false
	}
	start, err := time.Parse(time.RFC3339Nano, startTS)
	if err != nil {
		return false
	}
	return start.After(terminatedAt.Add(time.Second))
}
