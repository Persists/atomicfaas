package main

// All Redis access of the interceptor. Keys are shared with the detector and reverter:
// running:{pod} set, status:{invId}, timeline:{invId} status->ts, log:{invId} write-ahead
// effect list whose index == Effect.Seq, deferred:{invId} calls held for the commit,
// report:{invId} effects for a human, deadlines zset of aborted invocations.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// Written "attempted" before the call, rewritten in place as "committed" after.
type Effect struct {
	Seq         int    `json:"seq"`
	InvID       string `json:"invId"`
	TS          string `json:"ts"`
	Method      string `json:"method"`
	URL         string `json:"url"`
	Body        string `json:"body"`
	ContentType string `json:"contentType,omitempty"`
	RespStatus  int    `json:"respStatus,omitempty"`
	RespBody    string `json:"respBody,omitempty"`
	State       string `json:"state"`
}

type store struct {
	rdb *redis.Client
}

func newStore(addr string) *store {
	return &store{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// A finished invocation's records are kept this long for inspection; nothing reads them afterwards.
const retention = 10 * time.Minute

func expire(ctx context.Context, p redis.Pipeliner, invID string) {
	for _, k := range []string{"log:", "status:", "timeline:"} {
		p.Expire(ctx, k+invID, retention)
	}
}

// The detector reads the running stamp to tell an invocation of a dead container from one that started after it.
func stamp(ctx context.Context, p redis.Pipeliner, invID, status string) {
	p.Set(ctx, "status:"+invID, status, 0)
	p.HSet(ctx, "timeline:"+invID, status, now())
}

// A store write nothing reads back before the invocation ends; it rides on the
// next round trip. "attempted" is never queued: that is the write-ahead invariant.
type pendingWrite struct {
	key  string
	idx  int64 // >= 0: LSet at this index. < 0: RPush.
	blob []byte
}

func completionWrite(e Effect) (pendingWrite, error) {
	buf, err := json.Marshal(e)
	return pendingWrite{key: "log:" + e.InvID, idx: int64(e.Seq), blob: buf}, err
}

func deferredWrite(c deferredCall) (pendingWrite, error) {
	buf, err := json.Marshal(c)
	return pendingWrite{key: "deferred:" + c.InvID, idx: -1, blob: buf}, err
}

// LSet on distinct indices and RPush to another key: cannot disturb seq==list-index.
func applyPending(ctx context.Context, p redis.Pipeliner, ws []pendingWrite) {
	for _, w := range ws {
		if w.idx < 0 {
			p.RPush(ctx, w.key, w.blob)
		} else {
			p.LSet(ctx, w.key, w.idx, w.blob)
		}
	}
}

func (s *store) pushJSON(ctx context.Context, key string, v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.rdb.RPush(ctx, key, buf).Err()
}

func (s *store) MarkRunning(ctx context.Context, pod, invID string) error {
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.SAdd(ctx, "running:"+pod, invID)
		stamp(ctx, p, invID, "running")
		return nil
	})
	return err
}

// SRem only this invocation: a crashed sibling may still be waiting to be compensated.
func (s *store) MarkCommitted(ctx context.Context, pending []pendingWrite, pod, invID string) error {
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		applyPending(ctx, p, pending)
		p.Del(ctx, "deferred:"+invID)
		stamp(ctx, p, invID, "committed")
		p.SRem(ctx, "running:"+pod, invID)
		expire(ctx, p, invID)
		return nil
	})
	return err
}

// The one write that must be durable before its call goes out.
func (s *store) AppendAttempted(ctx context.Context, pending []pendingWrite, e Effect) error {
	buf, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		applyPending(ctx, p, pending)
		p.RPush(ctx, "log:"+e.InvID, buf)
		return nil
	})
	return err
}

// Deferred records must be durable before the calls they describe are sent.
func (s *store) FlushPending(ctx context.Context, pending []pendingWrite) error {
	if len(pending) == 0 {
		return nil
	}
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		applyPending(ctx, p, pending)
		return nil
	})
	return err
}

// Delivered before its invocation aborted: irreversible, so reported for a human.
func (s *store) ReportSent(ctx context.Context, c deferredCall) error {
	return s.pushJSON(ctx, "report:"+c.InvID, map[string]any{
		"seq": c.Seq, "method": c.Method, "url": c.URL,
		"state": "delivered", "reason": "delivered before abort; irreversible", "ts": now(),
	})
}

// Abort without a crash, marked like one: the reverter treats both alike. Settles the queue
// first: a completion left in memory would make a finished effect look mid-flight.
func (s *store) SelfAbort(ctx context.Context, pending []pendingWrite, pod, invID string) error {
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		applyPending(ctx, p, pending)
		stamp(ctx, p, invID, "crashed")
		p.ZAdd(ctx, "deadlines", redis.Z{Score: float64(time.Now().UnixMilli()) / 1000.0, Member: invID})
		p.SRem(ctx, "running:"+pod, invID)
		return nil
	})
	return err
}

// Backstop: runs before anything is served, so whatever is still "running" was
// left by the dead container. Racing the detector is harmless (ZAdd updates).
func (s *store) RecoverOrphans(ctx context.Context, pod string) ([]string, error) {
	key := "running:" + pod
	var queued []string
	err := s.rdb.Watch(ctx, func(tx *redis.Tx) error {
		invIDs, err := tx.SMembers(ctx, key).Result()
		if err != nil && err != redis.Nil {
			return err
		}
		queued = nil
		var handled []string
		for _, invID := range invIDs {
			status, err := tx.Get(ctx, "status:"+invID).Result()
			if err != nil && err != redis.Nil {
				return err
			}
			if status == "running" {
				queued = append(queued, invID)
			} else {
				handled = append(handled, invID) // the detector already has it
			}
		}
		if len(queued) == 0 && len(handled) == 0 {
			return nil
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
			score := float64(time.Now().UnixMilli()) / 1000.0
			for _, invID := range queued {
				stamp(ctx, p, invID, "crashed")
				p.ZAdd(ctx, "deadlines", redis.Z{Score: score, Member: invID})
			}
			for _, invID := range append(append([]string{}, queued...), handled...) {
				p.SRem(ctx, key, invID)
			}
			return nil
		})
		return err
	}, key)
	if err == redis.TxFailedErr {
		return nil, nil // the detector moved first; nothing left to do
	}
	return queued, err
}

func (s *store) Ping(ctx context.Context) error {
	return s.rdb.Ping(ctx).Err()
}
