package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/contextbuilder"
	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/history"
	"github.com/wbot-dev/wbot/internal/inference"
	"github.com/wbot-dev/wbot/internal/memory"
	"github.com/wbot-dev/wbot/internal/model"
	"github.com/wbot-dev/wbot/internal/permission"
	"github.com/wbot-dev/wbot/internal/storage"
	taskgraph "github.com/wbot-dev/wbot/internal/task"
	"github.com/wbot-dev/wbot/internal/tool"
)

type Service struct {
	s               config.Settings
	store           *storage.Store
	model           model.Generator
	tools           *tool.Registry
	memories        *memory.Manager
	history         *history.Manager
	context         *contextbuilder.Builder
	scheduler       *taskgraph.Scheduler
	planner         *Planner
	gate            taskCompletionGate
	collector       *EvidenceCollector
	running         sync.Map
	maintenanceOnce sync.Once
}

type toolExecution struct {
	call          domain.ToolCall
	result        domain.ToolResult
	approval      *domain.Approval
	originalBytes int
}

type taskCompletionGate interface {
	Evaluate(context.Context, domain.Task, string) (domain.GateResult, error)
	CompleteTask(context.Context, string, string) error
}

type replanRequested struct{ reason string }

type retryRequested struct{ reason string }

func (e *replanRequested) Error() string { return e.reason }
func (e *retryRequested) Error() string  { return e.reason }

const maxGraphRevisions = 4 // initial revision plus at most three replans

func New(s config.Settings, st *storage.Store, m model.Generator, t *tool.Registry, mem *memory.Manager, auxiliary ...inference.TextGenerator) *Service {
	hc := history.Config{Budget: s.MaxContextTokens / 4, MaxLoaded: s.History.MaxLoadedMessages, Recent: s.History.RecentMessages, RecentMin: s.History.RecentMinMessages, ReactiveRecent: s.History.ReactiveRecentMessages, SegmentMessages: s.History.SegmentMessages, SegmentMaxTokens: s.History.SegmentMaxSourceTokens, MergeFactor: s.History.SegmentMergeFactor, SummaryTarget: s.History.SummaryTargetTokens}
	opts := []history.Option{history.WithConfig(hc)}
	if len(auxiliary) > 0 && auxiliary[0] != nil {
		opts = append(opts, history.WithGenerator(auxiliary[0]))
	}
	hm := history.New(st, s.MaxContextTokens/4, opts...)
	registry := NewVerifierRegistry(permission.New(s, st))
	collector := NewEvidenceCollector(st, registry)
	gate := NewCompletionGate(st, collector)
	planner := NewPlanner(st)
	svc := &Service{s: s, store: st, model: m, tools: t, memories: mem, history: hm, scheduler: taskgraph.NewScheduler(s.MaxParallelism), planner: planner, gate: gate, collector: collector}
	svc.context = contextbuilder.New(s, st, mem, hm, t.Definitions)
	return svc
}
func (s *Service) Start(ctx context.Context, sessionID, objective string) (domain.Task, error) {
	if _, e := s.store.Session(ctx, sessionID); e != nil {
		return domain.Task{}, e
	}
	active, e := s.store.HasActiveTask(ctx, sessionID)
	if e != nil {
		return domain.Task{}, e
	}
	if active {
		return domain.Task{}, fmt.Errorf("当前会话仍有任务处理中，请等待最终回复")
	}
	t, e := s.store.CreateTask(ctx, sessionID, objective)
	if e != nil {
		return t, e
	}
	_, e = s.store.AddMessage(ctx, sessionID, t.ID, "user", objective)
	if e != nil {
		_ = s.store.UpdateTask(context.Background(), t.ID, "failed", "", e.Error())
		return t, e
	}
	// Use Planner with auto-detected complexity (P2: dynamic graph generation)
	complexity := DetectComplexity(t.Objective)
	nodes, criteria, revision, err := s.planner.GenerateGraph(ctx, t, complexity)
	if err != nil {
		_ = s.store.UpdateTask(context.Background(), t.ID, "failed", "", err.Error())
		return t, fmt.Errorf("graph generation failed: %w", err)
	}
	if e = taskgraph.Validate(nodes); e != nil {
		_ = s.store.UpdateTask(context.Background(), t.ID, "failed", "", e.Error())
		return t, e
	}
	if e = s.store.CreateGraphWithCriteria(ctx, t.ID, nodes, criteria, revision); e != nil {
		_ = s.store.UpdateTask(context.Background(), t.ID, "failed", "", e.Error())
		return t, e
	}
	s.store.Emit(ctx, sessionID, t.ID, "task.created", map[string]any{"task": t, "nodes": nodes, "revision": revision.Version})
	s.RunAsync(t.ID)
	return t, nil
}
func (s *Service) RunAsync(tid string) {
	ctx, cancel := context.WithCancel(context.Background())
	if _, loaded := s.running.LoadOrStore(tid, cancel); loaded {
		cancel()
		return
	}
	go func() { defer s.running.Delete(tid); defer cancel(); _ = s.Run(ctx, tid) }()
}
func (s *Service) Cancel(tid string) {
	if v, ok := s.running.Load(tid); ok {
		v.(context.CancelFunc)()
	}
}
func (s *Service) Recover(ctx context.Context) error {
	ids, e := s.store.RunningTasks(ctx)
	if e != nil {
		return e
	}
	for _, id := range ids {
		t, _ := s.store.Task(ctx, id)
		if t.Status == "running" {
			s.RunAsync(id)
		}
	}
	_ = s.store.EnqueueMaintenance(ctx, "memory.maintain", time.Now().UTC().Format("2006-01-02"), map[string]any{})
	s.startMaintenanceLoop()
	return nil
}

func (s *Service) startMaintenanceLoop() {
	s.maintenanceOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				_ = s.store.EnqueueMaintenance(context.Background(), "memory.maintain", time.Now().UTC().Format("2006-01-02"), map[string]any{})
				s.drainMaintenance(context.Background())
				<-ticker.C
			}
		}()
	})
}
func (s *Service) Run(ctx context.Context, tid string) error {
	t, e := s.store.Task(ctx, tid)
	if e != nil {
		return e
	}
	if t.Status == "waiting_approval" {
		return nil
	}

	for step := 0; step < 128; step++ {
		current, err := s.store.Task(ctx, tid)
		if err != nil {
			return s.fail(ctx, t, "", err)
		}
		if current.Status == "waiting_approval" || current.Status == "cancelled" {
			return nil
		}
		nodes, err := s.store.Nodes(ctx, tid)
		if err != nil {
			return s.fail(ctx, t, "", err)
		}
		ready := taskgraph.Ready(nodes)
		if len(ready) == 0 {
			recovered := false
			for _, node := range nodes {
				if node.Status == "running" || node.Status == "verifying" || (node.Status == "completed" && effectiveNodeKind(node) == domain.NodeVerify) {
					if err = s.store.UpdateNode(ctx, node.ID, "ready", node.Result); err != nil {
						return s.fail(ctx, t, node.ID, err)
					}
					recovered = true
					break
				}
			}
			if recovered {
				continue
			}
			return s.fail(ctx, t, "", fmt.Errorf("task graph is blocked: no ready nodes"))
		}

		var prep []domain.Node
		for _, n := range ready {
			if isPrepNode(n) {
				if err = s.store.UpdateNode(ctx, n.ID, "ready", ""); err != nil {
					return s.fail(ctx, t, n.ID, err)
				}
				prep = append(prep, n)
			}
		}
		if len(prep) > 0 {
			if err = s.runPreparation(ctx, t, prep); err != nil {
				return s.fail(ctx, t, "", err)
			}
			continue
		}

		node := ready[0]
		node.Kind = effectiveNodeKind(node)
		if err = s.store.UpdateNode(ctx, node.ID, "ready", ""); err != nil {
			return s.fail(ctx, t, node.ID, err)
		}
		node.Status = "ready"
		switch node.Kind {
		case domain.NodeResearch, domain.NodePlan, domain.NodeExecute:
			err = s.runModelNode(ctx, t, node)
			if request, ok := err.(*replanRequested); ok {
				if err = s.applyReplan(ctx, t, request.reason); err != nil {
					return s.fail(ctx, t, node.ID, err)
				}
				continue
			}
			if err != nil {
				return s.fail(ctx, t, node.ID, err)
			}
		case domain.NodeVerify:
			if err = s.runVerifyNode(ctx, t, node, nodes); err != nil {
				if _, ok := err.(*retryRequested); ok {
					continue
				}
				if request, ok := err.(*replanRequested); ok {
					if err = s.applyReplan(ctx, t, request.reason); err != nil {
						return s.fail(ctx, t, node.ID, err)
					}
					continue
				}
				return s.fail(ctx, t, node.ID, err)
			}
			return nil
		case domain.NodeApproval, domain.NodeWait:
			if err = s.store.UpdateTask(ctx, t.ID, "waiting_approval", "", node.Description); err != nil {
				return s.fail(ctx, t, node.ID, err)
			}
			return nil
		default:
			return s.fail(ctx, t, node.ID, fmt.Errorf("unsupported ready node kind %q", node.Kind))
		}
	}
	return s.fail(ctx, t, "", fmt.Errorf("task graph exceeded maximum scheduling steps"))
}

func isPrepNode(n domain.Node) bool {
	return n.Kind == domain.NodeLoadHist || n.Kind == domain.NodeRetrieve ||
		(n.Kind == "" && (n.Title == "加载会话历史" || n.Title == "检索长期记忆"))
}

func effectiveNodeKind(n domain.Node) domain.NodeKind {
	if n.Kind != "" {
		return n.Kind
	}
	switch n.Title {
	case "加载会话历史":
		return domain.NodeLoadHist
	case "检索长期记忆":
		return domain.NodeRetrieve
	case "执行目标", "执行变更":
		return domain.NodeExecute
	case "验收结果":
		return domain.NodeVerify
	}
	return ""
}

func (s *Service) runPreparation(ctx context.Context, t domain.Task, nodes []domain.Node) error {
	errs := s.scheduler.Run(ctx, nodes, func(n domain.Node) string { return n.Title }, func(c context.Context, n domain.Node) error {
		if err := s.store.TransitionNode(c, n.ID, "ready", "running", ""); err != nil {
			return err
		}
		var err error
		if n.Kind == domain.NodeLoadHist || n.Title == "加载会话历史" {
			_, _, err = s.history.Select(c, t.SessionID)
		} else {
			_, err = s.memories.Retrieve(c, t.Objective, 5)
		}
		if err != nil {
			return err
		}
		return s.store.TransitionNode(c, n.ID, "running", "completed", "prepared")
	})
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) runModelNode(ctx context.Context, t domain.Task, node domain.Node) error {
	if err := s.store.TransitionNode(ctx, node.ID, "ready", "running", ""); err != nil {
		return err
	}
	if err := s.store.SaveCheckpoint(ctx, t.ID, map[string]any{"phase": "model_loop", "node_id": node.ID, "node_kind": node.Kind}); err != nil {
		return err
	}
	if err := s.closeInterruptedToolGroup(ctx, t); err != nil {
		return err
	}
	for round := 0; round < 30; round++ {
		built, e := s.context.Build(ctx, contextbuilder.Request{SessionID: t.SessionID, TaskID: t.ID, Objective: node.Description, Mode: contextbuilder.Normal})
		if e != nil {
			return e
		}
		built.Messages = append(built.Messages, model.Message{Role: "system", Content: fmt.Sprintf("只执行当前节点 kind=%s title=%q。节点交付要求：%s。不得跳过依赖或宣称整个任务已完成。", node.Kind, node.Title, node.Description)})
		modelStarted := time.Now()
		resp, e := s.model.Generate(ctx, built.Messages, s.tools.Definitions())
		reactiveRetries := s.s.History.ReactiveRetryCount
		if reactiveRetries <= 0 {
			reactiveRetries = 1
		}
		if e != nil && reactiveRetries > 0 && (strings.Contains(strings.ToLower(e.Error()), "context") || strings.Contains(strings.ToLower(e.Error()), "maximum token")) {
			s.store.Emit(ctx, t.SessionID, t.ID, "context.reactive_retry", map[string]any{"normal_breakdown": built.Breakdown})
			reactive, x := s.context.Build(ctx, contextbuilder.Request{SessionID: t.SessionID, TaskID: t.ID, Objective: node.Description, Mode: contextbuilder.Reactive})
			if x != nil {
				e = x
			} else {
				reactive.Messages = append(reactive.Messages, model.Message{Role: "system", Content: fmt.Sprintf("只执行当前节点 kind=%s title=%q：%s", node.Kind, node.Title, node.Description)})
				resp, e = s.model.Generate(ctx, reactive.Messages, s.tools.Definitions())
				if e != nil && (strings.Contains(strings.ToLower(e.Error()), "context") || strings.Contains(strings.ToLower(e.Error()), "maximum token")) {
					e = fmt.Errorf("%w after reactive retry: breakdown=%+v: %v", contextbuilder.ErrBudgetExceeded, reactive.Breakdown, e)
					s.store.Emit(ctx, t.SessionID, t.ID, "context.budget_exceeded", map[string]any{"breakdown": reactive.Breakdown, "error": e.Error()})
				} else {
					s.store.Emit(ctx, t.SessionID, t.ID, "context.compacted", map[string]any{"mode": "reactive", "breakdown": reactive.Breakdown})
				}
			}
		}
		modelDurationMS := time.Since(modelStarted).Milliseconds()
		_ = s.store.RecordModelUsage(ctx, t.ID, s.s.DefaultModel.Name, "executor", resp.Usage, modelDurationMS)
		if e != nil {
			s.store.Emit(ctx, t.SessionID, t.ID, "model.failed", map[string]any{"duration_ms": modelDurationMS, "error": e.Error()})
			return e
		}
		s.store.Emit(ctx, t.SessionID, t.ID, "model.completed", map[string]any{"usage": resp.Usage, "tool_calls": len(resp.ToolCalls), "duration_ms": modelDurationMS})
		if len(resp.ToolCalls) == 0 {
			content := strings.TrimSpace(resp.Content)
			if content == "" {
				return fmt.Errorf("model returned empty response")
			}
			if node.Kind == domain.NodeExecute {
				taskMsgs, err := s.store.TaskMessages(ctx, t.ID, 200)
				if err != nil {
					return err
				}
				verification := evaluate(content, taskMsgs)
				for _, c := range verification.Criteria {
					if err = s.store.SetCriterion(ctx, t.ID, c.Criterion, c.Passed, c.Reason); err != nil {
						return err
					}
				}
				s.store.Emit(ctx, t.SessionID, t.ID, "task.verification", verification)
				if !verification.Passed {
					attempt, err := s.store.IncrementNodeAttempt(ctx, node.ID)
					if err != nil {
						return err
					}
					node.Attempt = attempt
					if taskgraph.ShouldReplan(node, taskgraph.ReplanNodeFailed, 2) {
						return &replanRequested{reason: fmt.Sprintf("node_failed: %s failed verification %d times", node.Title, attempt)}
					}
					if _, err = s.store.AddMessage(ctx, t.SessionID, t.ID, "user", "验收未通过，请修正后重新交付。原因：工具错误尚未解决"); err != nil {
						return err
					}
					continue
				}
				if _, err = s.store.AddMessage(ctx, t.SessionID, t.ID, "assistant", content); err != nil {
					return err
				}
			}
			return s.store.TransitionNode(ctx, node.ID, "running", "completed", content)
		}
		callsJSON, _ := json.Marshal(resp.ToolCalls)
		_, _ = s.store.AddStructuredMessage(ctx, domain.Message{SessionID: t.SessionID, TaskID: t.ID, Role: "assistant", Content: resp.Content, ContentJSON: callsJSON, Importance: .7})
		for callIndex := 0; callIndex < len(resp.ToolCalls); {
			end := callIndex + 1
			var executions []toolExecution
			if resp.ToolCalls[callIndex].Name == "filesystem.read" {
				firstArguments, _ := json.Marshal(resp.ToolCalls[callIndex].Arguments)
				seen := map[string]bool{string(firstArguments): true}
				for end < len(resp.ToolCalls) && resp.ToolCalls[end].Name == "filesystem.read" {
					arguments, _ := json.Marshal(resp.ToolCalls[end].Arguments)
					if seen[string(arguments)] {
						break
					}
					seen[string(arguments)] = true
					end++
				}
				executions = s.executeReadBatch(ctx, t, resp.ToolCalls[callIndex:end])
			} else {
				call := resp.ToolCalls[callIndex]
				s.store.Emit(ctx, t.SessionID, t.ID, "tool.started", call)
				executions = []toolExecution{s.executeTool(ctx, t, call)}
			}
			for offset, execution := range executions {
				currentIndex := callIndex + offset
				if e = s.persistToolExecution(ctx, t, execution); e != nil {
					return e
				}
				if execution.approval != nil {
					for _, skipped := range resp.ToolCalls[currentIndex+1:] {
						skippedResult := domain.ToolResult{ToolCallID: skipped.ID, Status: "skipped", Summary: "未执行：同批次工具调用正在等待审批", Retryable: true}
						raw, _ := json.Marshal(skippedResult)
						if _, x := s.store.AddStructuredMessage(ctx, domain.Message{SessionID: t.SessionID, TaskID: t.ID, Role: "tool", Content: skippedResult.Summary, ContentJSON: raw, ToolCallID: skipped.ID, ToolName: skipped.Name, Importance: .8}); x != nil {
							return x
						}
					}
					if e = s.store.UpdateNode(ctx, node.ID, "waiting_approval", ""); e != nil {
						return e
					}
					if e = s.store.UpdateTask(ctx, t.ID, "waiting_approval", "", ""); e != nil {
						return e
					}
					s.store.Emit(ctx, t.SessionID, t.ID, "approval.requested", execution.approval)
					return nil
				}
			}
			callIndex = end
		}
	}
	return fmt.Errorf("agent exceeded maximum rounds")
}

func (s *Service) runVerifyNode(ctx context.Context, t domain.Task, node domain.Node, nodes []domain.Node) error {
	if err := s.store.TransitionNode(ctx, node.ID, "ready", "running", ""); err != nil {
		return err
	}
	result, err := s.gate.Evaluate(ctx, t, node.ID)
	if err != nil {
		s.store.Emit(ctx, t.SessionID, t.ID, "gate.error", map[string]any{"error": err.Error()})
		return fmt.Errorf("completion gate failed closed: %w", err)
	}
	s.store.Emit(ctx, t.SessionID, t.ID, "gate.evaluated", result)
	switch result.Action {
	case domain.ActionComplete:
		content := ""
		for _, dep := range node.DependsOn {
			for _, candidate := range nodes {
				if candidate.ID == dep && candidate.Kind == domain.NodeExecute {
					content = candidate.Result
				}
			}
		}
		if err = s.store.TransitionNode(ctx, node.ID, "running", "completed", result.Reason); err != nil {
			return err
		}
		if err = s.gate.CompleteTask(ctx, t.ID, content); err != nil {
			return fmt.Errorf("completion gate commit failed: %w", err)
		}
		if err = s.store.SaveCheckpoint(ctx, t.ID, map[string]any{"phase": "completed"}); err != nil {
			return err
		}
		s.store.Emit(ctx, t.SessionID, t.ID, "task.completed", map[string]any{"result": content, "gate": result})
		_ = s.store.EnqueueMaintenance(ctx, "memory.extract", t.ID, map[string]any{"task_id": t.ID})
		go s.drainMaintenance(context.Background())
		return nil
	case domain.ActionRetry:
		attempt, attemptErr := s.store.IncrementNodeAttempt(ctx, node.ID)
		if attemptErr != nil {
			return attemptErr
		}
		if node.MaxAttempts > 0 && attempt >= node.MaxAttempts {
			return s.failVerification(ctx, t, node, fmt.Sprintf("verification retry limit reached: %s", result.Reason))
		}
		reset := false
		for _, dependencyID := range node.DependsOn {
			for _, candidate := range nodes {
				if candidate.ID == dependencyID && effectiveNodeKind(candidate) == domain.NodeExecute {
					if err = s.store.UpdateNode(ctx, candidate.ID, "ready", ""); err != nil {
						return err
					}
					reset = true
				}
			}
		}
		if !reset {
			return s.failVerification(ctx, t, node, "verification requested retry but has no executable dependency")
		}
		if err = s.store.UpdateNode(ctx, node.ID, "pending", result.Reason); err != nil {
			return err
		}
		return &retryRequested{reason: result.Reason}
	case domain.ActionReplan:
		return &replanRequested{reason: fmt.Sprintf("evaluator_%s: %s", result.Action, result.Reason)}
	case domain.ActionFail:
		return s.failVerification(ctx, t, node, result.Reason)
	case domain.ActionWaitForUser:
		if err = s.store.UpdateNode(ctx, node.ID, "waiting_external", result.Reason); err != nil {
			return err
		}
		return s.store.UpdateTask(ctx, t.ID, "waiting_approval", "", result.Reason)
	case domain.ActionWaitForExternal:
		if err = s.store.UpdateNode(ctx, node.ID, "waiting_external", result.Reason); err != nil {
			return err
		}
		return s.store.UpdateTask(ctx, t.ID, "waiting_external", "", result.Reason)
	default:
		return fmt.Errorf("completion gate returned unknown action %q", result.Action)
	}
}

func (s *Service) failVerification(ctx context.Context, t domain.Task, node domain.Node, reason string) error {
	if reason == "" {
		reason = "acceptance criteria failed"
	}
	if err := s.store.UpdateNode(ctx, node.ID, "failed", reason); err != nil {
		return err
	}
	if err := s.store.UpdateTask(ctx, t.ID, "failed", "", reason); err != nil {
		return err
	}
	s.store.Emit(ctx, t.SessionID, t.ID, "task.failed", map[string]any{"error": reason, "source": "completion_gate"})
	return nil
}

func (s *Service) applyReplan(ctx context.Context, t domain.Task, reason string) error {
	revisions, err := s.store.GraphRevisions(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(revisions) >= maxGraphRevisions {
		return fmt.Errorf("replan limit reached after %d graph revisions: %s", len(revisions), reason)
	}
	nodes, criteria, revision, err := s.planner.Replan(ctx, t, reason)
	if err != nil {
		return err
	}
	if err = s.store.ReplaceUnfinishedGraph(ctx, t.ID, nodes, criteria, revision); err != nil {
		return err
	}
	s.store.Emit(ctx, t.SessionID, t.ID, "task.replanned", map[string]any{"revision": revision.Version, "reason": reason, "nodes": nodes})
	return s.store.SaveCheckpoint(ctx, t.ID, map[string]any{"phase": "replan", "revision": revision.Version, "reason": reason})
}

func (s *Service) executeReadBatch(ctx context.Context, task domain.Task, calls []domain.ToolCall) []toolExecution {
	for _, call := range calls {
		s.store.Emit(ctx, task.SessionID, task.ID, "tool.started", call)
	}
	limit := s.s.MaxParallelism
	if limit < 1 {
		limit = 1
	}
	if limit > 8 {
		limit = 8
	}
	return executeToolBatch(ctx, calls, limit, func(callCtx context.Context, call domain.ToolCall) toolExecution {
		return s.executeTool(callCtx, task, call)
	})
}

func executeToolBatch(ctx context.Context, calls []domain.ToolCall, limit int, run func(context.Context, domain.ToolCall) toolExecution) []toolExecution {
	results := make([]toolExecution, len(calls))
	if len(calls) == 0 {
		return results
	}
	if limit < 1 {
		limit = 1
	}
	if limit > len(calls) {
		limit = len(calls)
	}
	indexes := make(chan int)
	var workers sync.WaitGroup
	workers.Add(limit)
	for worker := 0; worker < limit; worker++ {
		go func() {
			defer workers.Done()
			for index := range indexes {
				results[index] = run(ctx, calls[index])
			}
		}()
	}
	for index := range calls {
		indexes <- index
	}
	close(indexes)
	workers.Wait()
	return results
}

func (s *Service) executeTool(ctx context.Context, task domain.Task, call domain.ToolCall) toolExecution {
	// Hard per-tool timeout: prevents a hung tool from blocking the task forever.
	toolTimeout := 120 * time.Second
	toolCtx, toolCancel := context.WithTimeout(ctx, toolTimeout)
	defer toolCancel()

	type toolOut struct {
		result   domain.ToolResult
		approval *domain.Approval
	}
	done := make(chan toolOut, 1)
	go func() {
		r, a := s.tools.Execute(toolCtx, task.ID, call)
		done <- toolOut{result: r, approval: a}
	}()

	var result domain.ToolResult
	var approval *domain.Approval
	select {
	case out := <-done:
		result, approval = out.result, out.approval
	case <-toolCtx.Done():
		result = domain.ToolResult{
			ToolCallID: call.ID,
			Status:     "error",
			Summary:    fmt.Sprintf("工具调用超时（%v）", toolTimeout),
			Retryable:  true,
			Error:      &domain.ToolError{Code: "TOOL_TIMEOUT", Message: fmt.Sprintf("tool execution exceeded %v", toolTimeout)},
		}
		s.store.Emit(ctx, task.SessionID, task.ID, "tool.timeout", map[string]any{"tool": call.Name, "timeout": toolTimeout.String()})
	}
	rawResult, _ := json.Marshal(result)
	if len(rawResult) > 8*1024 {
		if aid, err := s.store.PutArtifact(ctx, task.ID, "application/json", rawResult); err == nil {
			result.Artifacts = append(result.Artifacts, aid)
		}
	}
	originalBytes := len(rawResult)
	result = history.ToolSnapshotFor(call.Name, result, s.s.History.ToolSnapshotMaxTokens*3)
	return toolExecution{call: call, result: result, approval: approval, originalBytes: originalBytes}
}

func (s *Service) persistToolExecution(ctx context.Context, task domain.Task, execution toolExecution) error {
	if execution.originalBytes > s.s.History.ToolSnapshotMaxTokens*3 {
		s.store.Emit(ctx, task.SessionID, task.ID, "tool.snapshot.created", map[string]any{"tool_call_id": execution.call.ID, "tool": execution.call.Name, "original_bytes": execution.originalBytes, "artifact_ids": execution.result.Artifacts})
	}
	raw, _ := json.Marshal(execution.result)
	if _, err := s.store.AddStructuredMessage(ctx, domain.Message{SessionID: task.SessionID, TaskID: task.ID, Role: "tool", Content: execution.result.Summary, ContentJSON: raw, ToolCallID: execution.call.ID, ToolName: execution.call.Name, ArtifactIDs: execution.result.Artifacts, Importance: .8}); err != nil {
		return err
	}
	if execution.approval != nil {
		return nil
	}
	s.store.Emit(ctx, task.SessionID, task.ID, "tool.completed", execution.result)
	return s.store.SaveCheckpoint(ctx, task.ID, map[string]any{"phase": "tool_completed", "tool_call": execution.call.ID})
}

func (s *Service) closeInterruptedToolGroup(ctx context.Context, task domain.Task) error {
	msgs, err := s.store.Messages(ctx, task.SessionID, 500)
	if err != nil || len(msgs) == 0 {
		return err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != "assistant" || len(m.ContentJSON) == 0 {
			continue
		}
		var calls []domain.ToolCall
		if json.Unmarshal(m.ContentJSON, &calls) != nil || len(calls) == 0 {
			continue
		}
		seen := map[string]bool{}
		for j := i + 1; j < len(msgs) && msgs[j].Role == "tool"; j++ {
			seen[msgs[j].ToolCallID] = true
		}
		if len(seen) == len(calls) {
			return nil
		}
		if i+1 < len(msgs) && msgs[len(msgs)-1].Role != "tool" {
			return fmt.Errorf("interrupted tool group is no longer contiguous")
		}
		for _, call := range calls {
			if seen[call.ID] {
				continue
			}
			result := domain.ToolResult{ToolCallID: call.ID, Status: "unknown", Summary: "进程中断：工具是否已产生外部影响无法确定，必须先检查实际状态", Retryable: false, Error: &domain.ToolError{Code: "RESULT_UNKNOWN", Message: "tool execution was interrupted before a durable result was recorded"}}
			raw, _ := json.Marshal(result)
			if _, err = s.store.AddStructuredMessage(ctx, domain.Message{SessionID: task.SessionID, TaskID: task.ID, Role: "tool", Content: result.Summary, ContentJSON: raw, ToolCallID: call.ID, ToolName: call.Name, Importance: 1}); err != nil {
				return err
			}
		}
		s.store.Emit(ctx, task.SessionID, task.ID, "tool.result_unknown", map[string]any{"assistant_message_id": m.ID})
		return nil
	}
	return nil
}
func (s *Service) Resume(ctx context.Context, tid string) {
	s.store.UpdateTask(ctx, tid, "running", "", "")
	nodes, _ := s.store.Nodes(ctx, tid)
	for _, n := range nodes {
		if n.Status == "waiting_approval" {
			_ = s.store.UpdateNode(ctx, n.ID, "ready", "")
		}
	}
	s.RunAsync(tid)
}
func (s *Service) fail(ctx context.Context, t domain.Task, nid string, e error) error {
	current, taskErr := s.store.Task(context.Background(), t.ID)
	if taskErr == nil && current.Status == "cancelled" {
		return e
	}
	s.store.UpdateNode(ctx, nid, "failed", e.Error())
	s.store.UpdateTask(ctx, t.ID, "failed", "", e.Error())
	s.store.Emit(ctx, t.SessionID, t.ID, "task.failed", map[string]any{"error": e.Error()})
	return e
}
func (s *Service) drainMaintenance(ctx context.Context) {
	for i := 0; i < 16; i++ {
		job, ok, e := s.store.ClaimMaintenance(ctx, 2*time.Minute)
		if e != nil || !ok {
			return
		}
		var runErr error
		switch job.Kind {
		case "memory.extract":
			var payload struct {
				TaskID string `json:"task_id"`
			}
			if e = json.Unmarshal([]byte(job.Payload), &payload); e != nil {
				runErr = e
				break
			}
			task, x := s.store.Task(ctx, payload.TaskID)
			if x != nil {
				runErr = x
				break
			}
			msgs, x := s.store.TaskMessages(ctx, payload.TaskID, 500)
			if x != nil {
				runErr = x
				break
			}
			workspace, _ := s.store.TaskWorkspaceRoot(ctx, payload.TaskID)
			runErr = s.memories.Extract(ctx, task, msgs, workspace)
			if runErr != nil {
				s.store.Emit(ctx, task.SessionID, task.ID, "memory.extraction_failed", map[string]any{"error": runErr.Error()})
			} else {
				s.store.Emit(ctx, task.SessionID, task.ID, "memory.candidate.extracted", map[string]any{"status": "completed"})
			}
		case "memory.maintain":
			runErr = s.memories.Maintain(ctx)
		default:
			runErr = fmt.Errorf("unknown maintenance job %s", job.Kind)
		}
		_ = s.store.FinishMaintenance(ctx, job.ID, runErr)
	}
}
