package main

// All Redis access of the reverter; keys are shared with the interceptor
// and the crash detector.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// Mirrors the schema written by the interceptor; read-only here.
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

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// The detector reads the running stamp to tell an invocation of a dead container from one that started after it.
func stamp(ctx context.Context, p redis.Pipeliner, invID, status string) {
	p.Set(ctx, "status:"+invID, status, 0)
	p.HSet(ctx, "timeline:"+invID, status, now())
}

func (s *store) Due(ctx context.Context) ([]string, error) {
	return s.rdb.ZRangeByScore(ctx, "deadlines", &redis.ZRangeBy{Min: "-inf", Max: "+inf"}).Result()
}

func (s *store) LoadLog(ctx context.Context, invID string) ([]Effect, error) {
	lines, err := s.rdb.LRange(ctx, "log:"+invID, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	effects := make([]Effect, 0, len(lines))
	for _, l := range lines {
		var e Effect
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			return nil, err
		}
		effects = append(effects, e)
	}
	return effects, nil
}

func (s *store) MarkCompensating(ctx context.Context, invID string) error {
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		stamp(ctx, p, invID, "compensating")
		return nil
	})
	return err
}

// A finished invocation's records are kept this long for inspection; nothing reads them afterwards.
const retention = 10 * time.Minute

func (s *store) MarkDone(ctx context.Context, invID, status string) error {
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		stamp(ctx, p, invID, status)
		p.ZRem(ctx, "deadlines", invID)
		for _, k := range []string{"log:", "status:", "timeline:", "report:"} {
			p.Expire(ctx, k+invID, retention)
		}
		return nil
	})
	return err
}

func (s *store) Report(ctx context.Context, invID string, entry any) error {
	buf, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return s.rdb.RPush(ctx, "report:"+invID, buf).Err()
}

// Calls held back for the commit. A crash during the flush leaves no record of which
// ones went out, so they are reported as possibly delivered before the key is dropped.
func (s *store) ReportDeferred(ctx context.Context, invID string) error {
	lines, err := s.rdb.LRange(ctx, "deferred:"+invID, 0, -1).Result()
	if err != nil {
		return err
	}
	_, err = s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		for _, l := range lines {
			var c map[string]any
			if json.Unmarshal([]byte(l), &c) != nil {
				continue
			}
			c["state"], c["reason"], c["ts"] = "unknown", "held for commit; may have been sent before the abort", now()
			buf, _ := json.Marshal(c)
			p.RPush(ctx, "report:"+invID, buf)
		}
		p.Del(ctx, "deferred:"+invID)
		return nil
	})
	return err
}
