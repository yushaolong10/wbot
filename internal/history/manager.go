package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/inference"
	"github.com/wbot-dev/wbot/internal/storage"
	"github.com/wbot-dev/wbot/internal/tokenizer"
)

type Config struct {
	Budget, MaxLoaded, Recent, RecentMin, ReactiveRecent          int
	SegmentMessages, SegmentMaxTokens, MergeFactor, SummaryTarget int
}

type Manager struct {
	store *storage.Store
	cfg   Config
	aux   inference.TextGenerator
	tok   tokenizer.Counter
	locks sync.Map
}

type Option func(*Manager)

func WithGenerator(g inference.TextGenerator) Option { return func(m *Manager) { m.aux = g } }
func WithConfig(c Config) Option                     { return func(m *Manager) { m.cfg = normalize(c) } }

func normalize(c Config) Config {
	if c.Budget < 1000 {
		c.Budget = 1000
	}
	if c.MaxLoaded < 100 {
		c.MaxLoaded = 500
	}
	if c.Recent < 4 {
		c.Recent = 12
	}
	if c.RecentMin < 1 {
		c.RecentMin = 4
	}
	if c.ReactiveRecent < 2 {
		c.ReactiveRecent = 6
	}
	if c.SegmentMessages < 4 {
		c.SegmentMessages = 30
	}
	if c.SegmentMaxTokens < 1000 {
		c.SegmentMaxTokens = 12000
	}
	if c.MergeFactor < 2 {
		c.MergeFactor = 4
	}
	if c.SummaryTarget < 200 {
		c.SummaryTarget = 800
	}
	return c
}

func New(st *storage.Store, budget int, opts ...Option) *Manager {
	m := &Manager{store: st, cfg: normalize(Config{Budget: budget})}
	for _, o := range opts {
		o(m)
	}
	return m
}

func (m *Manager) lock(sid string) func() {
	v, _ := m.locks.LoadOrStore(sid, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Select preserves the old small interface while using persistent hierarchical
// segments. It is also useful to API callers that only need a rendered summary.
func (m *Manager) Select(ctx context.Context, sid string) (string, []domain.Message, error) {
	summary, recent, _, err := m.SelectDetailed(ctx, sid)
	return summary, recent, err
}

func (m *Manager) Force(ctx context.Context, sid string, keep int) (string, []domain.Message, error) {
	summary, recent, _, err := m.ForceDetailed(ctx, sid, keep)
	return summary, recent, err
}

func (m *Manager) SelectDetailed(ctx context.Context, sid string) (string, []domain.Message, []string, error) {
	return m.prepare(ctx, sid, m.cfg.Recent, m.cfg.Budget)
}

func (m *Manager) ForceDetailed(ctx context.Context, sid string, keep int) (string, []domain.Message, []string, error) {
	if keep < 1 {
		keep = m.cfg.ReactiveRecent
	}
	return m.prepare(ctx, sid, keep, maxInt(500, m.cfg.Budget/2))
}

func (m *Manager) prepare(ctx context.Context, sid string, recentCount, budget int) (string, []domain.Message, []string, error) {
	unlock := m.lock(sid)
	defer unlock()
	all, e := m.store.Messages(ctx, sid, m.cfg.MaxLoaded)
	if e != nil {
		return "", nil, nil, e
	}
	if len(all) == 0 {
		return "", all, nil, nil
	}
	recentCount = maxInt(recentCount, m.cfg.RecentMin)
	total := 0
	for _, x := range all {
		total += m.messageTokens(x)
	}
	truncatedByLoadLimit := all[0].Seq > 1
	if total <= budget && !truncatedByLoadLimit && len(all) <= m.cfg.SegmentMessages {
		return "", all, nil, nil
	}
	if recentCount >= len(all) {
		recentCount = maxInt(1, len(all)/3)
	}
	start := len(all) - recentCount
	start = preserveToolBoundary(all, start)
	cutoff := all[start].Seq - 1
	if e = m.ensureLevelZero(ctx, sid, cutoff); e != nil {
		return "", nil, nil, e
	}
	if e = m.mergeSegments(ctx, sid); e != nil {
		return "", nil, nil, e
	}
	segments, e := m.store.HistorySegments(ctx, sid, "active")
	if e != nil {
		return "", nil, nil, e
	}
	summary, ids := m.renderFrontier(segments, budget/3)
	recent := all[start:]
	return summary, recent, ids, nil
}

func preserveToolBoundary(all []domain.Message, start int) int {
	if start < 0 {
		return 0
	}
	for start > 0 && all[start].Role == "tool" {
		start--
	}
	// Keep any unresolved call group, regardless of age.
	for i := 0; i < start; i++ {
		if all[i].Role != "assistant" || len(all[i].ContentJSON) == 0 {
			continue
		}
		var calls []domain.ToolCall
		if json.Unmarshal(all[i].ContentJSON, &calls) != nil || len(calls) == 0 {
			continue
		}
		seen := map[string]bool{}
		for j := i + 1; j < len(all) && all[j].Role == "tool"; j++ {
			seen[all[j].ToolCallID] = true
		}
		if len(seen) < len(calls) {
			start = i
			break
		}
	}
	return start
}

func (m *Manager) ensureLevelZero(ctx context.Context, sid string, cutoff int64) error {
	segments, e := m.store.HistorySegments(ctx, sid, "")
	if e != nil {
		return e
	}
	covered := int64(0)
	for _, s := range segments {
		if s.Level == 0 && s.LastSeq > covered {
			covered = s.LastSeq
		}
	}
	for covered < cutoff {
		pending, err := m.store.MessagesRange(ctx, sid, covered, cutoff, m.cfg.SegmentMessages)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			break
		}
		end, tokens := 0, 0
		for end < len(pending) && end < m.cfg.SegmentMessages {
			n := m.messageTokens(pending[end])
			if end > 0 && tokens+n > m.cfg.SegmentMaxTokens {
				break
			}
			tokens += n
			end++
		}
		if end == 0 {
			end = 1
		}
		for i := 0; i < end; i++ {
			if pending[i].Role != "assistant" || len(pending[i].ContentJSON) == 0 {
				continue
			}
			var calls []domain.ToolCall
			if json.Unmarshal(pending[i].ContentJSON, &calls) != nil || len(calls) == 0 {
				continue
			}
			groupEnd := i + 1
			for groupEnd < len(pending) && pending[groupEnd].Role == "tool" {
				groupEnd++
			}
			if groupEnd-i-1 != len(calls) || groupEnd > end {
				end = i
				break
			}
			i = groupEnd - 1
		}
		if end == 0 {
			break
		}
		chunk := pending[:end]
		// A tool-call group is indivisible. If the token/count boundary lands in
		// the group, either include the complete group or leave it for Recent.
		if chunk[0].Role == "tool" {
			return fmt.Errorf("history segment would start with orphan tool result at seq %d", chunk[0].Seq)
		}
		seg, err := m.summarizeMessages(ctx, sid, chunk)
		if err != nil {
			return err
		}
		unchanged, err := m.sourceUnchanged(ctx, sid, chunk, seg.SourceHash)
		if err != nil {
			return err
		}
		if !unchanged {
			return fmt.Errorf("history source changed before segment commit")
		}
		if err = m.store.SaveHistorySegment(ctx, seg); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "unique") {
				return err
			}
		} else {
			_ = m.store.SetMessagesCompactionState(ctx, sid, seg.FirstSeq, seg.LastSeq, "summarized")
			_, _ = m.store.Emit(ctx, sid, seg.TaskID, "history.segment.created", map[string]any{"segment_id": seg.ID, "first_seq": seg.FirstSeq, "last_seq": seg.LastSeq, "tokens": seg.TokenCount, "source_hash": seg.SourceHash})
		}
		covered = seg.LastSeq
	}
	return nil
}

func (m *Manager) sourceUnchanged(ctx context.Context, sid string, original []domain.Message, wantHash string) (bool, error) {
	current, err := m.store.MessagesRange(ctx, sid, original[0].Seq-1, original[len(original)-1].Seq, len(original)+1)
	if err != nil {
		return false, err
	}
	b, _ := json.Marshal(current)
	h := sha256.Sum256(b)
	return len(current) == len(original) && hex.EncodeToString(h[:]) == wantHash, nil
}

func (m *Manager) summarizeMessages(ctx context.Context, sid string, msgs []domain.Message) (domain.HistorySegment, error) {
	b, _ := json.Marshal(msgs)
	sum := sha256.Sum256(b)
	summary := deterministicSummary(msgs)
	modelName := "deterministic"
	if m.aux != nil {
		system := `你负责压缩 Agent 历史。只输出 JSON 对象，字段必须是 objectives,user_constraints,verified_facts,decisions,completed_actions,pending_actions,failed_actions,active_tool_calls,artifacts,memory_ids,file_changes,open_questions，所有字段均为字符串数组。不得把失败写成成功；保留约束、未完成事项、路径和引用 ID；不要推测。`
		auxCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		out, generateErr := m.aux.Complete(auxCtx, system, string(b))
		cancel()
		if generateErr == nil {
			var parsed domain.HistorySummary
			if decodeSummary(extractJSON(out), &parsed) == nil {
				summary = parsed
				modelName = "auxiliary"
			}
		}
	}
	summary = fitSummary(capSummary(summary), m.cfg.SummaryTarget, m.tok)
	sb, _ := json.Marshal(summary)
	ids := make([]string, len(msgs))
	for i := range msgs {
		ids[i] = msgs[i].ID
	}
	return domain.HistorySegment{SessionID: sid, TaskID: msgs[len(msgs)-1].TaskID, Level: 0, FirstSeq: msgs[0].Seq, LastSeq: msgs[len(msgs)-1].Seq, SourceMessageIDs: ids, SourceMessageCount: len(msgs), SummaryJSON: string(sb), TokenCount: m.tok.CountString(string(sb)), Model: modelName, PromptVersion: "1", SourceHash: hex.EncodeToString(sum[:]), Status: "active"}, nil
}

func (m *Manager) mergeSegments(ctx context.Context, sid string) error {
	for level := 0; level < 32; level++ {
		segments, e := m.store.HistorySegments(ctx, sid, "active")
		if e != nil {
			return e
		}
		var same []domain.HistorySegment
		for _, s := range segments {
			if s.Level == level {
				same = append(same, s)
			}
		}
		sort.Slice(same, func(i, j int) bool { return same[i].FirstSeq < same[j].FirstSeq })
		for len(same) >= m.cfg.MergeFactor {
			group := same[:m.cfg.MergeFactor]
			merged, err := m.summarizeSegments(ctx, sid, group)
			if err != nil {
				return err
			}
			ids := make([]string, len(group))
			for i := range group {
				ids[i] = group[i].ID
			}
			if err = m.store.CompactHistorySegments(ctx, ids, merged); err != nil {
				if !strings.Contains(strings.ToLower(err.Error()), "unique") {
					return err
				}
			}
			_, _ = m.store.Emit(ctx, sid, merged.TaskID, "history.segment.merged", map[string]any{"segment_id": merged.ID, "source_segment_ids": ids, "level": merged.Level, "tokens": merged.TokenCount})
			same = same[m.cfg.MergeFactor:]
		}
	}
	return nil
}

func (m *Manager) summarizeSegments(ctx context.Context, sid string, src []domain.HistorySegment) (domain.HistorySegment, error) {
	b, _ := json.Marshal(src)
	h := sha256.Sum256(b)
	var summaries []domain.HistorySummary
	for _, s := range src {
		var x domain.HistorySummary
		_ = json.Unmarshal([]byte(s.SummaryJSON), &x)
		summaries = append(summaries, x)
	}
	merged := mergeSummary(summaries)
	modelName := "deterministic"
	if m.aux != nil {
		system := `合并多个历史摘要。只输出与输入相同字段的 JSON 字符串数组。保留所有未完成事项、用户约束、失败、路径及引用，去重已验证事实；不得推测或宣称未验证成功。`
		auxCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		out, generateErr := m.aux.Complete(auxCtx, system, string(b))
		cancel()
		if generateErr == nil {
			var x domain.HistorySummary
			if decodeSummary(extractJSON(out), &x) == nil {
				merged = x
				modelName = "auxiliary"
			}
		}
	}
	merged = fitSummary(capSummary(merged), m.cfg.SummaryTarget, m.tok)
	sb, _ := json.Marshal(merged)
	ids := make([]string, len(src))
	count := 0
	for i := range src {
		ids[i] = src[i].ID
		count += src[i].SourceMessageCount
	}
	return domain.HistorySegment{SessionID: sid, TaskID: src[len(src)-1].TaskID, Level: src[0].Level + 1, FirstSeq: src[0].FirstSeq, LastSeq: src[len(src)-1].LastSeq, SourceSegmentIDs: ids, SourceMessageCount: count, SummaryJSON: string(sb), TokenCount: m.tok.CountString(string(sb)), Model: modelName, PromptVersion: "1", SourceHash: hex.EncodeToString(h[:]), Status: "active"}, nil
}

func (m *Manager) renderFrontier(segments []domain.HistorySegment, budget int) (string, []string) {
	sort.Slice(segments, func(i, j int) bool { return segments[i].FirstSeq < segments[j].FirstSeq })
	if len(segments) == 0 {
		return "", nil
	}
	ids := make([]string, 0, len(segments))
	summaries := make([]domain.HistorySummary, 0, len(segments))
	for _, s := range segments {
		ids = append(ids, s.ID)
		var parsed domain.HistorySummary
		if json.Unmarshal([]byte(s.SummaryJSON), &parsed) == nil {
			summaries = append(summaries, parsed)
		}
	}
	frontier := fitSummary(mergeSummary(summaries), maxInt(100, budget-100), m.tok)
	encoded, _ := json.Marshal(frontier)
	var b strings.Builder
	b.WriteString("<history_summary>\n")
	fmt.Fprintf(&b, "segments=%s range=%d-%d\n%s\n", strings.Join(ids, ","), segments[0].FirstSeq, segments[len(segments)-1].LastSeq, encoded)
	b.WriteString("</history_summary>")
	return b.String(), ids
}

func (m *Manager) messageTokens(x domain.Message) int {
	if x.TokenCount > 0 {
		return x.TokenCount
	}
	return m.tok.CountString(x.Content) + m.tok.CountJSON(x.ContentJSON)
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func Snip(s string, maxRunes int) string {
	s = ansi.ReplaceAllString(strings.ReplaceAll(s, "\r\n", "\n"), "")
	lines := strings.Split(s, "\n")
	var out []string
	for i := 0; i < len(lines); {
		j := i + 1
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		out = append(out, lines[i])
		if j-i > 2 {
			out = append(out, fmt.Sprintf("[上一行重复 %d 次]", j-i-1))
		}
		i = j
	}
	r := []rune(strings.Join(out, "\n"))
	if maxRunes > 0 && len(r) > maxRunes {
		return string(r[:maxRunes/2]) + "\n…[snipped]…\n" + string(r[len(r)-maxRunes/2:])
	}
	return string(r)
}

func ToolSnapshot(result domain.ToolResult, maxRunes int) domain.ToolResult {
	return ToolSnapshotFor("", result, maxRunes)
}

func ToolSnapshotFor(toolName string, result domain.ToolResult, maxRunes int) domain.ToolResult {
	b, _ := json.Marshal(result.Data)
	if len([]rune(string(b))) <= maxRunes {
		return result
	}
	snapshot := map[string]any{"tool": toolName, "status": result.Status, "retryable": result.Retryable, "artifact_ids": result.Artifacts}
	data, _ := result.Data.(map[string]any)
	switch toolName {
	case "filesystem.read":
		if content, ok := data["content"].(string); ok {
			snapshot["excerpt"] = Snip(content, maxRunes)
		} else {
			snapshot["preview"] = Snip(string(b), maxRunes)
		}
	case "filesystem.write":
		for _, key := range []string{"path", "bytes", "sha256"} {
			if value, ok := data[key]; ok {
				snapshot[key] = value
			}
		}
	case "shell.execute":
		if output, ok := data["output"].(string); ok {
			snapshot["important_output"] = Snip(output, maxRunes)
		} else {
			snapshot["preview"] = Snip(string(b), maxRunes)
		}
	default:
		snapshot["preview"] = Snip(string(b), maxRunes)
	}
	result.Data = snapshot
	if result.Summary == "" {
		result.Summary = "工具结果已压缩"
	}
	return result
}

func deterministicSummary(msgs []domain.Message) domain.HistorySummary {
	var s domain.HistorySummary
	for _, m := range msgs {
		c := Snip(m.Content, 400)
		switch m.Role {
		case "user":
			s.Objectives = append(s.Objectives, c)
		case "tool":
			var result domain.ToolResult
			_ = json.Unmarshal(m.ContentJSON, &result)
			if result.Status == "error" || result.Error != nil {
				s.FailedActions = append(s.FailedActions, c)
			} else if result.Status == "success" || result.Status == "completed" {
				s.CompletedActions = append(s.CompletedActions, c)
			} else {
				s.ActiveToolCalls = append(s.ActiveToolCalls, m.ToolCallID)
			}
		case "assistant":
			if len(m.ContentJSON) > 0 {
				var calls []domain.ToolCall
				if json.Unmarshal(m.ContentJSON, &calls) == nil {
					for _, call := range calls {
						s.ActiveToolCalls = append(s.ActiveToolCalls, call.ID)
					}
				}
			} else if c != "" {
				s.PendingActions = append(s.PendingActions, c)
			}
		}
	}
	return capSummary(s)
}

func decodeSummary(raw []byte, out *domain.HistorySummary) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	fields := [][]string{out.Objectives, out.UserConstraints, out.VerifiedFacts, out.Decisions, out.CompletedActions, out.PendingActions, out.FailedActions, out.ActiveToolCalls, out.Artifacts, out.MemoryIDs, out.FileChanges, out.OpenQuestions}
	for _, f := range fields {
		if f == nil {
			return errors.New("history summary omitted a required field")
		}
	}
	return nil
}
func mergeSummary(xs []domain.HistorySummary) domain.HistorySummary {
	var o domain.HistorySummary
	for _, x := range xs {
		o.Objectives = append(o.Objectives, x.Objectives...)
		o.UserConstraints = append(o.UserConstraints, x.UserConstraints...)
		o.VerifiedFacts = append(o.VerifiedFacts, x.VerifiedFacts...)
		o.Decisions = append(o.Decisions, x.Decisions...)
		o.CompletedActions = append(o.CompletedActions, x.CompletedActions...)
		o.PendingActions = append(o.PendingActions, x.PendingActions...)
		o.FailedActions = append(o.FailedActions, x.FailedActions...)
		o.ActiveToolCalls = append(o.ActiveToolCalls, x.ActiveToolCalls...)
		o.Artifacts = append(o.Artifacts, x.Artifacts...)
		o.MemoryIDs = append(o.MemoryIDs, x.MemoryIDs...)
		o.FileChanges = append(o.FileChanges, x.FileChanges...)
		o.OpenQuestions = append(o.OpenQuestions, x.OpenQuestions...)
	}
	return capSummary(o)
}
func uniqCap(xs []string, n int) []string {
	seen := map[string]bool{}
	o := make([]string, 0, min(n, len(xs)))
	for i := len(xs) - 1; i >= 0 && len(o) < n; i-- {
		x := strings.TrimSpace(xs[i])
		if x != "" && !seen[x] {
			seen[x] = true
			o = append(o, x)
		}
	}
	for i, j := 0, len(o)-1; i < j; i, j = i+1, j-1 {
		o[i], o[j] = o[j], o[i]
	}
	return o
}
func capSummary(s domain.HistorySummary) domain.HistorySummary {
	s.Objectives = uniqCap(s.Objectives, 12)
	s.UserConstraints = uniqCap(s.UserConstraints, 30)
	s.VerifiedFacts = uniqCap(s.VerifiedFacts, 40)
	s.Decisions = uniqCap(s.Decisions, 30)
	s.CompletedActions = uniqCap(s.CompletedActions, 20)
	s.PendingActions = uniqCap(s.PendingActions, 30)
	s.FailedActions = uniqCap(s.FailedActions, 20)
	s.ActiveToolCalls = uniqCap(s.ActiveToolCalls, 20)
	s.Artifacts = uniqCap(s.Artifacts, 30)
	s.MemoryIDs = uniqCap(s.MemoryIDs, 30)
	s.FileChanges = uniqCap(s.FileChanges, 30)
	s.OpenQuestions = uniqCap(s.OpenQuestions, 20)
	return s
}

func fitSummary(s domain.HistorySummary, target int, tok tokenizer.Counter) domain.HistorySummary {
	trim := func(xs []string) []string {
		for i := range xs {
			xs[i] = Snip(xs[i], 300)
		}
		return xs
	}
	s.Objectives, s.UserConstraints, s.VerifiedFacts = trim(s.Objectives), trim(s.UserConstraints), trim(s.VerifiedFacts)
	s.Decisions, s.CompletedActions, s.PendingActions = trim(s.Decisions), trim(s.CompletedActions), trim(s.PendingActions)
	s.FailedActions, s.ActiveToolCalls, s.Artifacts = trim(s.FailedActions), trim(s.ActiveToolCalls), trim(s.Artifacts)
	s.MemoryIDs, s.FileChanges, s.OpenQuestions = trim(s.MemoryIDs), trim(s.FileChanges), trim(s.OpenQuestions)
	if target <= 0 {
		return s
	}
	for tok.CountJSON(s) > target {
		switch {
		case len(s.CompletedActions) > 0:
			s.CompletedActions = s.CompletedActions[1:]
		case len(s.VerifiedFacts) > 0:
			s.VerifiedFacts = s.VerifiedFacts[1:]
		case len(s.Objectives) > 1:
			s.Objectives = s.Objectives[1:]
		case len(s.FileChanges) > 0:
			s.FileChanges = s.FileChanges[1:]
		case len(s.Artifacts) > 0:
			s.Artifacts = s.Artifacts[1:]
		case len(s.MemoryIDs) > 0:
			s.MemoryIDs = s.MemoryIDs[1:]
		case len(s.Decisions) > 1:
			s.Decisions = s.Decisions[1:]
		default:
			return s
		}
	}
	return s
}
func extractJSON(s string) []byte {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return []byte(s[start : end+1])
	}
	return []byte(s)
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
