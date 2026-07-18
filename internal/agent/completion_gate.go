package agent

import (
	"context"
	"fmt"

	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/storage"
)

// CompletionGate is the ONLY path through which a task can be marked "completed".
// Neither the Executor nor the model may directly complete a task.
type CompletionGate struct {
	store     *storage.Store
	collector *EvidenceCollector
}

func NewCompletionGate(store *storage.Store, collector *EvidenceCollector) *CompletionGate {
	return &CompletionGate{store: store, collector: collector}
}

// Evaluate runs the full completion check:
// 1. Collect deterministic evidence for all criteria
// 2. Check that all required criteria passed
// 3. Deterministic failures cannot be overridden
// 4. Return the gate action
func (g *CompletionGate) Evaluate(ctx context.Context, task domain.Task, nodeID string) (domain.GateResult, error) {
	criteria, err := g.store.AcceptanceCriteria(ctx, task.ID)
	if err != nil {
		return domain.GateResult{}, err
	}
	if len(criteria) == 0 {
		// Migrate legacy criteria into the durable v2 contract before evaluating,
		// so evidence always references a persisted criterion ID.
		criteria, err = g.store.Criteria(ctx, task.ID)
		if err != nil {
			return domain.GateResult{}, err
		}
		if len(criteria) > 0 {
			if err = g.store.CreateAcceptanceCriteriaBatch(ctx, criteria); err != nil {
				return domain.GateResult{}, fmt.Errorf("migrate legacy acceptance criteria: %w", err)
			}
			criteria, err = g.store.AcceptanceCriteria(ctx, task.ID)
			if err != nil {
				return domain.GateResult{}, err
			}
		}
	}
	if len(criteria) == 0 {
		return domain.GateResult{}, fmt.Errorf("task has no acceptance criteria")
	}

	// Collect deterministic evidence
	evidence, updatedCriteria, err := g.collector.Collect(ctx, nodeID, criteria)
	if err != nil {
		return domain.GateResult{}, fmt.Errorf("evidence collection failed: %w", err)
	}

	result := domain.GateResult{Action: domain.ActionComplete}
	result.TotalCount = len(updatedCriteria)

	for _, c := range updatedCriteria {
		switch c.Status {
		case "passed":
			result.PassedCount++
		case "failed":
			result.FailedCount++
			if c.Required {
				result.Action = domain.ActionFail
				result.Reason = fmt.Sprintf("required criterion %q (%s) failed: %s", c.Description, c.Type, c.Reason)
			}
		default:
			// pending — not yet evaluated
			if c.Required {
				result.Action = domain.ActionRetry
				if result.Reason == "" {
					result.Reason = fmt.Sprintf("required criterion %q is still pending", c.Description)
				}
			}
		}
	}

	// If any deterministic evidence failed, model cannot override
	for _, ev := range evidence {
		if ev.Type == "deterministic" && !ev.Passed {
			// A deterministic failure may be repairable by executing the node
			// again, but it can never be converted into success by a model-only
			// judgment. The runtime enforces the retry limit.
			result.Action = domain.ActionRetry
			result.Reason = fmt.Sprintf("deterministic verifier failed: %s", ev.Summary)
		}
	}

	if result.Action == domain.ActionComplete && result.PassedCount == result.TotalCount {
		result.Reason = "all criteria passed"
	}

	return result, nil
}

// CompleteTask writes the completed status. Only this method should be called
// to finalize a task — never call store.UpdateTask(completed) directly.
func (g *CompletionGate) CompleteTask(ctx context.Context, taskID, result string) error {
	return g.store.UpdateTask(ctx, taskID, "completed", result, "")
}
