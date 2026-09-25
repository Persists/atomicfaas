package main

// Termination detection. Reads both State.Terminated and LastTerminationState
// (the kubelet restarts so fast the former is usually gone); dedupes on
// ContainerID and never evicts, or the persisting LastTerminationState would re-fire.

import (
	"context"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

type detector struct {
	handoff *handoff

	mu   sync.Mutex
	seen map[string]struct{}
}

func newDetector(h *handoff) *detector {
	return &detector{handoff: h, seen: map[string]struct{}{}}
}

func (d *detector) onUpdate(_, obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "user-container" { // ignore queue-proxy
			continue
		}
		d.checkTermination(pod.Name, cs.State.Terminated, "State")
		d.checkTermination(pod.Name, cs.LastTerminationState.Terminated, "LastTerminationState")
	}
}

func (d *detector) checkTermination(podName string, t *corev1.ContainerStateTerminated, source string) {
	if t == nil {
		return
	}
	key := t.ContainerID
	if key == "" { // containerID can lag briefly, fall back to a stable enough key
		key = podName + "/" + t.FinishedAt.String()
	}
	d.mu.Lock()
	_, dup := d.seen[key]
	d.seen[key] = struct{}{}
	d.mu.Unlock()
	if dup {
		return
	}
	if t.ExitCode == 0 { // graceful shutdown / clean drain is not a crash
		return
	}
	log.Printf("crash: pod=%s exit=%d reason=%q source=%s", podName, t.ExitCode, t.Reason, source)
	d.handoff.run(context.Background(), podName, t.FinishedAt.Time)
}

// A deleted pod (revision GC, drain, kubectl delete) is a crash for its in-flight invocation.
func (d *detector) onDelete(obj any) {
	if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = unknown.Obj
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	deletedAt := time.Now() // DeletionTimestamp can be absent on stale tombstones
	if pod.DeletionTimestamp != nil {
		deletedAt = pod.DeletionTimestamp.Time
	}
	log.Printf("crash: pod=%s reason=deleted", pod.Name)
	d.handoff.run(context.Background(), pod.Name, deletedAt)
}
