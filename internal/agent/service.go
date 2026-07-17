package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/history"
	"github.com/wbot-dev/wbot/internal/memory"
	"github.com/wbot-dev/wbot/internal/model"
	"github.com/wbot-dev/wbot/internal/storage"
	taskgraph "github.com/wbot-dev/wbot/internal/task"
	"github.com/wbot-dev/wbot/internal/tool"
)

type Service struct {
	s         config.Settings
	store     *storage.Store
	model     model.Generator
	tools     *tool.Registry
	memories  *memory.Manager
	history   *history.Manager
	scheduler *taskgraph.Scheduler
	running   sync.Map
}

func New(s config.Settings, st *storage.Store, m model.Generator, t *tool.Registry, mem *memory.Manager) *Service {
	return &Service{s: s, store: st, model: m, tools: t, memories: mem, history: history.New(st, s.MaxContextTokens/4), scheduler: taskgraph.NewScheduler(s.MaxParallelism)}
}
func (s *Service) Start(ctx context.Context, sessionID, objective string) (domain.Task, error) {
	if _, e := s.store.Session(ctx, sessionID); e != nil {
		return domain.Task{}, e
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
	return nil
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
	for round := 0; round < 30; round++ {
		msgs, e := s.buildContext(ctx, t)
		if e != nil {
			return s.fail(ctx, t, nodeID, e)
		}
		resp, e := s.model.Generate(ctx, msgs, s.tools.Definitions())
		if e != nil && (strings.Contains(strings.ToLower(e.Error()), "context") || strings.Contains(strings.ToLower(e.Error()), "maximum token")) {
			summary, recent, x := s.history.Force(ctx, t.SessionID, 6)
			if x == nil {
				compact := []model.Message{msgs[0], {Role: "system", Content: summary}}
				for _, m := range recent {
					role := m.Role
					if role == "tool" {
						role = "user"
					}
					compact = append(compact, model.Message{Role: role, Content: m.Content})
				}
				resp, e = s.model.Generate(ctx, compact, s.tools.Definitions())
				s.store.Emit(ctx, t.SessionID, t.ID, "context.compacted", map[string]any{"mode": "reactive", "messages": len(recent)})
			}
		}
		if e != nil {
			return s.fail(ctx, t, nodeID, e)
		}
		_ = s.store.RecordModelUsage(ctx, t.ID, s.s.DefaultModel.Name, "executor", resp.Usage)
		s.store.Emit(ctx, t.SessionID, t.ID, "model.completed", map[string]any{"usage": resp.Usage, "tool_calls": len(resp.ToolCalls)})
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
			return nil
		}
		if resp.Content != "" {
			s.store.AddMessage(ctx, t.SessionID, t.ID, "assistant", resp.Content)
		}
		for _, call := range resp.ToolCalls {
			b, _ := json.Marshal(call)
			s.store.AddMessage(ctx, t.SessionID, t.ID, "assistant", "TOOL_CALL "+string(b))
			s.store.Emit(ctx, t.SessionID, t.ID, "tool.started", call)
			result, approval := s.tools.Execute(ctx, t.ID, call)
			rb, _ := json.Marshal(result)
			s.store.AddMessage(ctx, t.SessionID, t.ID, "tool", string(rb))
			if approval != nil {
				_ = s.store.UpdateNode(ctx, nodeID, "waiting_approval", "")
				s.store.UpdateTask(ctx, t.ID, "waiting_approval", "", "")
				s.store.Emit(ctx, t.SessionID, t.ID, "approval.requested", approval)
				return nil
			}
			s.store.Emit(ctx, t.SessionID, t.ID, "tool.completed", result)
			_ = s.store.SaveCheckpoint(ctx, t.ID, map[string]any{"phase": "tool_completed", "tool_call": call.ID})
		}
	}
	return s.fail(ctx, t, nodeID, fmt.Errorf("agent exceeded maximum rounds"))
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
	s.store.UpdateNode(ctx, nid, "failed", e.Error())
	s.store.UpdateTask(ctx, t.ID, "failed", "", e.Error())
	s.store.Emit(ctx, t.SessionID, t.ID, "task.failed", map[string]any{"error": e.Error()})
	return e
}
func (s *Service) buildContext(ctx context.Context, t domain.Task) ([]model.Message, error) {
	p, b, e := config.LoadProfile(s.s.ProfilePath)
	if e != nil {
		return nil, e
	}
	h := sha256.Sum256(b)
	s.store.Emit(ctx, t.SessionID, t.ID, "prompt.built", map[string]any{"profile_version": p.Version, "profile_hash": "sha256:" + hex.EncodeToString(h[:]), "prompt_template_version": "0.1", "estimated_tokens": len(b) / 3})
	mem, _ := s.memories.Retrieve(ctx, t.Objective, 5)
	summary, hist, e := s.history.Select(ctx, t.SessionID)
	if e != nil {
		return nil, e
	}
	system := fmt.Sprintf("你是 %s，角色是%s。语言：%s。风格：%s。\n%s\n必须通过工具获取证据；所有副作用由权限引擎裁决；不得声称未验证的成功。当前工作区：%s。Profile hash: sha256:%s", p.Identity.Name, p.Identity.Role, p.Identity.Language, p.Personality.Tone, p.CustomInstructions, s.s.WorkspaceRoot, hex.EncodeToString(h[:]))
	if len(mem) > 0 {
		mb, _ := json.Marshal(mem)
		system += "\n相关长期记忆：" + string(mb)
	}
	out := []model.Message{{Role: "system", Content: system}}
	if summary != "" {
		out = append(out, model.Message{Role: "system", Content: summary})
	}
	for _, m := range hist {
		role := m.Role
		if role == "tool" {
			var tr domain.ToolResult
			if json.Unmarshal([]byte(m.Content), &tr) == nil {
				out = append(out, model.Message{Role: "tool", ToolCallID: tr.ToolCallID, Content: m.Content})
				continue
			}
			role = "user"
			m.Content = "上一工具结果：" + m.Content
		}
		if strings.HasPrefix(m.Content, "TOOL_CALL ") {
			var tc domain.ToolCall
			if json.Unmarshal([]byte(strings.TrimPrefix(m.Content, "TOOL_CALL ")), &tc) == nil {
				args, _ := json.Marshal(tc.Arguments)
				out = append(out, model.Message{Role: "assistant", ToolCalls: []any{map[string]any{"id": tc.ID, "type": "function", "function": map[string]any{"name": tool.ModelName(tc.Name), "arguments": string(args)}}}})
				continue
			}
			role = "user"
			m.Content = "此前模型请求：" + m.Content
		}
		out = append(out, model.Message{Role: role, Content: m.Content})
	}
	return out, nil
}
