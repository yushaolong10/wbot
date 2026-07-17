package task

import (
	"context"
	"github.com/wbot-dev/wbot/internal/domain"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateCycle(t *testing.T) {
	a := domain.Node{ID: "a", DependsOn: []string{"b"}}
	b := domain.Node{ID: "b", DependsOn: []string{"a"}}
	if Validate([]domain.Node{a, b}) == nil {
		t.Fatal("expected cycle error")
	}
}
func TestSchedulerParallelAndResourceLock(t *testing.T) {
	s := NewScheduler(2)
	nodes := []domain.Node{{ID: "a", Title: "r1"}, {ID: "b", Title: "r2"}}
	var active, max int32
	s.Run(context.Background(), nodes, func(n domain.Node) string { return n.Title }, func(context.Context, domain.Node) error {
		n := atomic.AddInt32(&active, 1)
		for {
			m := atomic.LoadInt32(&max)
			if n <= m || atomic.CompareAndSwapInt32(&max, m, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return nil
	})
	if max != 2 {
		t.Fatalf("expected parallelism 2, got %d", max)
	}
	max = 0
	s.Run(context.Background(), nodes, func(domain.Node) string { return "same" }, func(context.Context, domain.Node) error {
		n := atomic.AddInt32(&active, 1)
		if n > atomic.LoadInt32(&max) {
			atomic.StoreInt32(&max, n)
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return nil
	})
	if max != 1 {
		t.Fatalf("resource lock failed: %d", max)
	}
}
