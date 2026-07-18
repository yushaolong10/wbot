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
	running         sync.Map
	maintenanceOnce sync.Once
}

type toolExecution struct {
	call          domain.ToolCall
	result        domain.ToolResult
	approval      *domain.Approval
	originalBytes int
}

func New(s config.Settings, st *storage.Store, m model.Generator, t *tool.Registry, mem *memory.Manager, auxiliary ...inference.TextGenerator) *Service {
	hc := history.Config{Budget: s.MaxContextTokens / 4, MaxLoaded: s.History.MaxLoadedMessages, Recent: s.History.RecentMessages, RecentMin: s.History.RecentMinMessages, ReactiveRecent: s.History.ReactiveRecentMessages, SegmentMessages: s.History.SegmentMessages, SegmentMaxTokens: s.History.SegmentMaxSourceTokens, MergeFactor: s.History.SegmentMergeFactor, SummaryTarget: s.History.SummaryTargetTokens}
	opts := []history.Option{history.WithConfig(hc)}
	if len(auxiliary) > 0 && auxiliary[0] != nil {
		opts = append(opts, history.WithGenerator(auxiliary[0]))
	}
	hm := history.New(st, s.MaxContextTokens/4, opts...)
	svc := &Service{s: s, store: st, model: m, tools: t, memories: mem, history: hm, scheduler: taskgraph.NewScheduler(s.MaxParallelism)}
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
		return t, e
	}
	h := storage.NewID("node")
	m := storage.NewID("node")
	x := storage.NewID("node")
	v := storage.NewID("node")
	nodes := []domain.Node{{ID: h, TaskID: t.ID, Title: "加载会话历史", Description: "在上下文预算内加载并压缩历史", Status: "pending", MaxAttempts: 2}, {ID: m, TaskID: t.ID, Title: "检索长期记忆", Description: "按任务语义检索记忆", Status: "pending", MaxAttempts: 2}, {ID: x, TaskID: t.ID, Title: "执行目标", Description: objective, DependsOn: []string{h, m}, Status: "pending", MaxAttempts: 2}, {ID: v, TaskID: t.ID, Title: "验收结果", Description: "按顶层标准验证交付结果", DependsOn: []string{x}, Status: "pending", MaxAttempts: 2}}
	if e = taskgraph.Validate(nodes); e != nil {
		return t, e
	}
	if e = s.store.CreateGraph(ctx, t.ID, nodes); e != nil {
		return t, e
	}
	s.store.Emit(ctx, sessionID, t.ID, "task.created", map[string]any{"task": t, "nodes": nodes})
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
	nodes, _ := s.store.Nodes(ctx, tid)
	nodeID := ""
	verifyID := ""
	for _, n := range nodes {
		if n.Title == "执行目标" {
			nodeID = n.ID
		}
		if n.Title == "验收结果" {
			verifyID = n.ID
		}
	}
	if t.Status == "waiting_approval" {
		return nil
	}
	ready := taskgraph.Ready(nodes)
	prep := []domain.Node{}
	for _, n := range ready {
		if n.Title == "加载会话历史" || n.Title == "检索长期记忆" {
			_ = s.store.UpdateNode(ctx, n.ID, "ready", "")
			prep = append(prep, n)
		}
	}
	errs := s.scheduler.Run(ctx, prep, func(n domain.Node) string { return n.Title }, func(c context.Context, n domain.Node) error {
		_ = s.store.TransitionNode(c, n.ID, "ready", "running", "")
		var e error
		if n.Title == "加载会话历史" {
			_, _, e = s.history.Select(c, t.SessionID)
		} else {
			_, e = s.memories.Retrieve(c, t.Objective, 5)
		}
		if e == nil {
			e = s.store.TransitionNode(c, n.ID, "running", "completed", "prepared")
		}
		return e
	})
	for _, e := range errs {
		if e != nil {
			return s.fail(ctx, t, nodeID, e)
		}
	}
	_ = s.store.UpdateNode(ctx, nodeID, "ready", "")
	_ = s.store.TransitionNode(ctx, nodeID, "ready", "running", "")
	_ = s.store.SaveCheckpoint(ctx, t.ID, map[string]any{"phase": "model_loop", "node_id": nodeID})
	if e = s.closeInterruptedToolGroup(ctx, t); e != nil {
		return s.fail(ctx, t, nodeID, e)
	}
	for round := 0; round < 30; round++ {
		built, e := s.context.Build(ctx, contextbuilder.Request{SessionID: t.SessionID, TaskID: t.ID, Objective: t.Objective, Mode: contextbuilder.Normal})
		if e != nil {
			return s.fail(ctx, t, nodeID, e)
		}
		modelStarted := time.Now()
		resp, e := s.model.Generate(ctx, built.Messages, s.tools.Definitions())
		reactiveRetries := s.s.History.ReactiveRetryCount
		if reactiveRetries <= 0 {
			reactiveRetries = 1
		}
		if e != nil && reactiveRetries > 0 && (strings.Contains(strings.ToLower(e.Error()), "context") || strings.Contains(strings.ToLower(e.Error()), "maximum token")) {
			s.store.Emit(ctx, t.SessionID, t.ID, "context.reactive_retry", map[string]any{"normal_breakdown": built.Breakdown})
			reactive, x := s.context.Build(ctx, contextbuilder.Request{SessionID: t.SessionID, TaskID: t.ID, Objective: t.Objective, Mode: contextbuilder.Reactive})
			if x != nil {
				e = x
			} else {
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
			return s.fail(ctx, t, nodeID, e)
		}
		s.store.Emit(ctx, t.SessionID, t.ID, "model.completed", map[string]any{"usage": resp.Usage, "tool_calls": len(resp.ToolCalls), "duration_ms": modelDurationMS})
		if len(resp.ToolCalls) == 0 {
			content := strings.TrimSpace(resp.Content)
			if content == "" {
				return s.fail(ctx, t, nodeID, fmt.Errorf("model returned empty response"))
			}
			taskMsgs, _ := s.store.TaskMessages(ctx, t.ID, 200)
			verification := evaluate(content, taskMsgs)
			for _, c := range verification.Criteria {
				_ = s.store.SetCriterion(ctx, t.ID, c.Criterion, c.Passed, c.Reason)
			}
			s.store.Emit(ctx, t.SessionID, t.ID, "task.verification", verification)
			if !verification.Passed {
				s.store.AddMessage(ctx, t.SessionID, t.ID, "user", "验收未通过，请修正后重新交付。原因：工具错误尚未解决")
				_ = s.store.SaveCheckpoint(ctx, t.ID, map[string]any{"phase": "replan", "round": round})
				continue
			}
			s.store.AddMessage(ctx, t.SessionID, t.ID, "assistant", content)
			_ = s.store.TransitionNode(ctx, nodeID, "running", "verifying", content)
			_ = s.store.TransitionNode(ctx, nodeID, "verifying", "completed", content)
			_ = s.store.UpdateNode(ctx, verifyID, "ready", "")
			_ = s.store.TransitionNode(ctx, verifyID, "ready", "running", "")
			_ = s.store.TransitionNode(ctx, verifyID, "running", "completed", "all criteria passed")
			s.store.UpdateTask(ctx, t.ID, "completed", content, "")
			_ = s.store.SaveCheckpoint(ctx, t.ID, map[string]any{"phase": "completed"})
			s.store.Emit(ctx, t.SessionID, t.ID, "task.completed", map[string]any{"result": content, "verification": verification})
			_ = s.store.EnqueueMaintenance(ctx, "memory.extract", t.ID, map[string]any{"task_id": t.ID})
			go s.drainMaintenance(context.Background())
			return nil
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
					return s.fail(ctx, t, nodeID, e)
				}
				if execution.approval != nil {
					// Chat completion APIs require one result for every call in the
					// assistant's tool_calls array. Mark unstarted siblings explicitly.
					for _, skipped := range resp.ToolCalls[currentIndex+1:] {
						skippedResult := domain.ToolResult{ToolCallID: skipped.ID, Status: "skipped", Summary: "未执行：同批次工具调用正在等待审批", Retryable: true}
						raw, _ := json.Marshal(skippedResult)
						if _, x := s.store.AddStructuredMessage(ctx, domain.Message{SessionID: t.SessionID, TaskID: t.ID, Role: "tool", Content: skippedResult.Summary, ContentJSON: raw, ToolCallID: skipped.ID, ToolName: skipped.Name, Importance: .8}); x != nil {
							return s.fail(ctx, t, nodeID, x)
						}
					}
					_ = s.store.UpdateNode(ctx, nodeID, "waiting_approval", "")
					s.store.UpdateTask(ctx, t.ID, "waiting_approval", "", "")
					s.store.Emit(ctx, t.SessionID, t.ID, "approval.requested", execution.approval)
					return nil
				}
			}
			callIndex = end
		}
	}
	return s.fail(ctx, t, nodeID, fmt.Errorf("agent exceeded maximum rounds"))
}

func (s *Service) executeReadBatch(ctx context.Context, task domain.Task, calls []domain.ToolCall) []toolExecution {
	for _, call := range calls {
		s.store.Emit(ctx, task.SessionID, task.ID, "tool.started", call)
	}
	limit := s.s.MaxParallelism
	if limit < 1 {
		limit = 1
	}
	if limit > 4 {
		limit = 4
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
	result, approval := s.tools.Execute(ctx, task.ID, call)
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
			_ = s.store.UpdateNode(ctx, n.ID, "running", "")
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
