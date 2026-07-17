package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/wbot-dev/wbot/internal/config"
	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/memory"
	"github.com/wbot-dev/wbot/internal/permission"
	"github.com/wbot-dev/wbot/internal/storage"
)

type Definition struct {
	Name, Description string
	Parameters        map[string]any
}
type Registry struct {
	s            config.Settings
	store        *storage.Store
	permissions  *permission.Engine
	memories     *memory.Manager
	advisor      Advisor
	mu           sync.Mutex
	advisorCalls map[string]int
}
type Advisor interface {
	Consult(context.Context, string, string) (string, error)
}

func New(s config.Settings, st *storage.Store, p *permission.Engine, m *memory.Manager, a Advisor) *Registry {
	if s.AdvisorMaxCalls < 1 {
		s.AdvisorMaxCalls = 3
	}
	return &Registry{s: s, store: st, permissions: p, memories: m, advisor: a, advisorCalls: map[string]int{}}
}
func (r *Registry) Definitions() []Definition {
	return []Definition{
		{"filesystem.read", "读取授权工作区内的文本文件", obj(map[string]any{"path": str("文件路径")}, "path")},
		{"filesystem.write", "创建或覆盖授权工作区内的文件", obj(map[string]any{"path": str("文件路径"), "content": str("文件内容")}, "path", "content")},
		{"shell.execute", "在授权工作区内运行命令", obj(map[string]any{"command": str("命令"), "timeout_seconds": map[string]any{"type": "integer"}}, "command")},
		{"memory.save", "保存经过验证、未来可复用的长期记忆", obj(map[string]any{"type": str("user/project/episodic/procedural"), "summary": str("摘要"), "content": str("内容"), "tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "type", "summary", "content")},
		{"consult_advisor", "咨询高能力模型，返回建议但不执行操作", obj(map[string]any{"problem": str("问题"), "expected_output": str("期望输出"), "relevant_context": str("必要上下文")}, "problem", "expected_output")},
	}
}

// ModelName converts an internal, human-readable tool name into the portable
// function-name subset accepted by OpenAI-compatible model APIs.
func ModelName(name string) string {
	var b strings.Builder
	for _, ch := range name {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' {
			b.WriteRune(ch)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func obj(p map[string]any, req ...string) map[string]any {
	return map[string]any{"type": "object", "properties": p, "required": req, "additionalProperties": false}
}
func str(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
func (r *Registry) Execute(ctx context.Context, taskID string, c domain.ToolCall) (domain.ToolResult, *domain.Approval) {
	start := time.Now()
	res := domain.ToolResult{ToolCallID: c.ID, Status: "success"}
	if err := r.validate(c); err != nil {
		return failed(c.ID, "INVALID_ARGUMENTS", err), nil
	}
	d, err := r.permissions.Evaluate(ctx, taskID, c.Name, c.Arguments)
	if err != nil {
		return failed(c.ID, "PERMISSION_ERROR", err), nil
	}
	if d.Kind == "DENY" {
		return failed(c.ID, "PERMISSION_DENIED", fmt.Errorf("%s", d.Reason)), nil
	}
	if d.Kind == "ASK" {
		a, e := r.store.CreateApproval(ctx, taskID, "", c.Name, c.Arguments, d.Risk, d.Reason)
		if e != nil {
			return failed(c.ID, "APPROVAL_ERROR", e), nil
		}
		res.Status = "waiting_approval"
		res.Summary = "等待审批"
		return res, &a
	}
	digest := permission.Digest(c.Arguments)
	status, cached, found, lookupErr := r.store.ToolCall(ctx, taskID, c.Name, digest)
	if lookupErr != nil {
		return failed(c.ID, "IDEMPOTENCY_ERROR", lookupErr), nil
	}
	if found {
		if status == "completed" || status == "failed" {
			var prior domain.ToolResult
			if json.Unmarshal([]byte(cached), &prior) == nil {
				return prior, nil
			}
		}
		return failed(c.ID, "RESULT_UNKNOWN", fmt.Errorf("an identical tool call is already %s; inspect external state before retrying", status)), nil
	}
	if c.ID == "" {
		c.ID = storage.NewID("call")
	}
	if err := r.store.BeginToolCall(ctx, c.ID, taskID, "", c.Name, digest); err != nil {
		return failed(c.ID, "IDEMPOTENCY_ERROR", err), nil
	}
	switch c.Name {
	case "filesystem.read":
		p, e := r.permissions.ResolveTaskPath(ctx, taskID, asString(c.Arguments["path"]))
		if e == nil {
			var b []byte
			b, e = os.ReadFile(p)
			if e == nil {
				if len(b) > 64*1024 {
					aid, x := r.store.PutArtifact(ctx, taskID, "text/plain", b)
					e = x
					res.Data = map[string]any{"artifact_id": aid, "preview": string(b[:8192])}
					res.Artifacts = []string{aid}
					res.Summary = "文件较大，完整内容已保存为 Artifact"
				} else {
					res.Data = map[string]any{"content": string(b)}
					res.Summary = "文件已读取"
				}
			}
		}
		err = e
	case "filesystem.write":
		p, e := r.permissions.ResolveTaskPath(ctx, taskID, asString(c.Arguments["path"]))
		if e == nil {
			e = os.MkdirAll(filepathDir(p), 0755)
			if e == nil {
				e = atomicWrite(p, []byte(asString(c.Arguments["content"])), 0644)
			}
		}
		err = e
		res.Summary = "文件已写入"
		res.Data = map[string]any{"path": p}
	case "shell.execute":
		sec := 30
		if n, ok := c.Arguments["timeout_seconds"].(float64); ok && n > 0 && n <= 300 {
			sec = int(n)
		}
		cctx, cancel := context.WithTimeout(ctx, time.Duration(sec)*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, "/bin/sh", "-lc", asString(c.Arguments["command"]))
		cmd.Dir = r.permissions.WorkspaceRoot(ctx, taskID)
		var out bytes.Buffer
		cmd.Stdout = &limitedWriter{w: &out, n: 256 * 1024}
		cmd.Stderr = &limitedWriter{w: &out, n: 256 * 1024}
		err = cmd.Run()
		res.Data = map[string]any{"output": out.String()}
		res.Summary = "命令执行完成"
	case "memory.save":
		tags := []string{}
		if a, ok := c.Arguments["tags"].([]any); ok {
			for _, v := range a {
				tags = append(tags, asString(v))
			}
		}
		err = r.memories.Upsert(ctx, memory.Entry{Type: asString(c.Arguments["type"]), Summary: asString(c.Arguments["summary"]), Content: asString(c.Arguments["content"]), Tags: tags, Confidence: .8, Importance: .7})
		res.Summary = "记忆已保存"
	case "consult_advisor":
		if r.advisor == nil {
			err = fmt.Errorf("advisor is not configured")
		} else {
			r.mu.Lock()
			if r.advisorCalls[taskID] >= r.s.AdvisorMaxCalls {
				r.mu.Unlock()
				err = fmt.Errorf("advisor call limit reached")
				break
			}
			r.advisorCalls[taskID]++
			r.mu.Unlock()
			var s string
			s, err = r.advisor.Consult(ctx, redact(asString(c.Arguments["problem"])), redact(asString(c.Arguments["expected_output"])+"\n"+asString(c.Arguments["relevant_context"])))
			res.Data = map[string]any{"analysis_summary": s, "recommendation": s, "risks": []string{}, "suggested_next_steps": []string{}, "confidence": 0.5}
			res.Summary = "Advisor 已返回建议"
		}
	default:
		err = fmt.Errorf("unknown tool %q", c.Name)
	}
	if err != nil {
		res = failed(c.ID, "TOOL_FAILED", err)
	}
	res.DurationMS = time.Since(start).Milliseconds()
	callStatus := "completed"
	if res.Status == "error" {
		callStatus = "failed"
	}
	_ = r.store.CompleteToolCall(ctx, c.ID, callStatus, res)
	return res, nil
}
func (r *Registry) validate(c domain.ToolCall) error {
	for _, d := range r.Definitions() {
		if d.Name != c.Name {
			continue
		}
		req, _ := d.Parameters["required"].([]string)
		for _, k := range req {
			v, ok := c.Arguments[k]
			if !ok || v == nil || v == "" {
				return fmt.Errorf("%s is required", k)
			}
		}
		return nil
	}
	return fmt.Errorf("unknown tool %q", c.Name)
}
func redact(s string) string {
	for _, marker := range []string{"sk-", "Bearer ", "PRIVATE KEY", "password=", "token="} {
		if i := strings.Index(strings.ToLower(s), strings.ToLower(marker)); i >= 0 {
			end := i + len(marker)
			for end < len(s) && s[end] != ' ' && s[end] != '\n' {
				end++
			}
			s = s[:i] + "[REDACTED]" + s[end:]
		}
	}
	return s
}
func atomicWrite(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".wbot.tmp"
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	if x := f.Close(); e == nil {
		e = x
	}
	if e != nil {
		os.Remove(tmp)
		return e
	}
	if e = os.Rename(tmp, path); e != nil {
		return e
	}
	if d, x := os.Open(filepathDir(path)); x == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
func failed(id, code string, e error) domain.ToolResult {
	return domain.ToolResult{ToolCallID: id, Status: "error", Error: &domain.ToolError{Code: code, Message: e.Error()}, Summary: e.Error()}
}
func asString(v any) string { s, _ := v.(string); return s }
func filepathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

type limitedWriter struct {
	w io.Writer
	n int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if int64(len(q)) > l.n {
		q = q[:l.n]
	}
	n, e := l.w.Write(q)
	l.n -= int64(n)
	return len(p), e
}

var _ = json.Marshal
