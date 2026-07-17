package task

import (
	"context"
	"fmt"
	"sync"

	"github.com/wbot-dev/wbot/internal/domain"
)

var validTransitions = map[string]map[string]bool{
	"pending": {"ready": true, "cancelled": true}, "ready": {"running": true, "cancelled": true},
	"running":          {"verifying": true, "completed": true, "waiting_approval": true, "waiting_external": true, "failed": true, "cancelled": true},
	"waiting_approval": {"ready": true, "failed": true, "cancelled": true}, "waiting_external": {"ready": true, "failed": true, "cancelled": true},
	"verifying": {"completed": true, "failed": true, "ready": true}, "failed": {"ready": true, "cancelled": true},
}

func CanTransition(from, to string) bool { return validTransitions[from][to] }
func Validate(nodes []domain.Node) error {
	by := map[string]domain.Node{}
	for _, n := range nodes {
		if n.ID == "" {
			return fmt.Errorf("node id is required")
		}
		if _, ok := by[n.ID]; ok {
			return fmt.Errorf("duplicate node %s", n.ID)
		}
		by[n.ID] = n
	}
	for _, n := range nodes {
		for _, d := range n.DependsOn {
			if _, ok := by[d]; !ok {
				return fmt.Errorf("node %s depends on missing %s", n.ID, d)
			}
		}
	}
	visiting, done := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("task graph contains a cycle at %s", id)
		}
		if done[id] {
			return nil
		}
		visiting[id] = true
		for _, d := range by[id].DependsOn {
			if e := visit(d); e != nil {
				return e
			}
		}
		visiting[id] = false
		done[id] = true
		return nil
	}
	for id := range by {
		if e := visit(id); e != nil {
			return e
		}
	}
	return nil
}
func Ready(nodes []domain.Node) []domain.Node {
	completed := map[string]bool{}
	for _, n := range nodes {
		completed[n.ID] = n.Status == "completed"
	}
	var out []domain.Node
	for _, n := range nodes {
		if n.Status != "pending" && n.Status != "ready" {
			continue
		}
		ok := true
		for _, d := range n.DependsOn {
			ok = ok && completed[d]
		}
		if ok {
			n.Status = "ready"
			out = append(out, n)
		}
	}
	return out
}

type Work func(context.Context, domain.Node) error
type Scheduler struct {
	parallel int
	locks    sync.Map
}

func NewScheduler(n int) *Scheduler {
	if n < 1 {
		n = 1
	}
	return &Scheduler{parallel: n}
}
func (s *Scheduler) Run(ctx context.Context, nodes []domain.Node, resource func(domain.Node) string, work Work) map[string]error {
	sem := make(chan struct{}, s.parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	errs := map[string]error{}
	for _, n := range nodes {
		n := n
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				errs[n.ID] = ctx.Err()
				mu.Unlock()
				return
			}
			defer func() { <-sem }()
			key := resource(n)
			var lock *sync.Mutex
			if key != "" {
				v, _ := s.locks.LoadOrStore(key, &sync.Mutex{})
				lock = v.(*sync.Mutex)
				lock.Lock()
				defer lock.Unlock()
			}
			e := work(ctx, n)
			mu.Lock()
			errs[n.ID] = e
			mu.Unlock()
		}()
	}
	wg.Wait()
	return errs
}
