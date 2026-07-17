package history

import (
	"context"
	"github.com/wbot-dev/wbot/internal/domain"
	"github.com/wbot-dev/wbot/internal/storage"
	"strings"
)

type Manager struct {
	store  *storage.Store
	budget int
}

func New(st *storage.Store, budget int) *Manager {
	if budget < 1000 {
		budget = 1000
	}
	return &Manager{st, budget}
}
func estimate(s string) int { return len([]rune(s))/3 + 1 }
func (m *Manager) Select(ctx context.Context, sid string) (string, []domain.Message, error) {
	all, e := m.store.Messages(ctx, sid, 200)
	if e != nil {
		return "", nil, e
	}
	tokens := 0
	for _, x := range all {
		tokens += estimate(x.Content)
	}
	if tokens <= m.budget {
		return "", all, nil
	}
	keep := 12
	if len(all) <= keep {
		return "", all, nil
	}
	old, recent := all[:len(all)-keep], all[len(all)-keep:]
	last, existing, e := m.store.LatestSummary(ctx, sid)
	if e != nil {
		return "", nil, e
	}
	if last == old[len(old)-1].ID {
		return existing, recent, nil
	}
	var b strings.Builder
	b.WriteString("历史摘要：\n")
	for _, x := range old {
		content := strings.ReplaceAll(x.Content, "\n", " ")
		r := []rune(content)
		if len(r) > 240 {
			content = string(r[:240]) + "…"
		}
		b.WriteString("- " + x.Role + ": " + content + "\n")
	}
	summary := b.String()
	_ = m.store.SaveSummary(ctx, sid, old[0].ID, old[len(old)-1].ID, summary)
	return summary, recent, nil
}
func (m *Manager) Force(ctx context.Context, sid string, keep int) (string, []domain.Message, error) {
	all, e := m.store.Messages(ctx, sid, 200)
	if e != nil {
		return "", nil, e
	}
	if keep < 1 {
		keep = 6
	}
	if len(all) <= keep {
		return "", all, nil
	}
	old, recent := all[:len(all)-keep], all[len(all)-keep:]
	var b strings.Builder
	b.WriteString("紧急压缩历史摘要：\n")
	for _, x := range old {
		content := strings.ReplaceAll(x.Content, "\n", " ")
		r := []rune(content)
		if len(r) > 160 {
			content = string(r[:160]) + "…"
		}
		b.WriteString("- " + x.Role + ": " + content + "\n")
	}
	summary := b.String()
	_ = m.store.SaveSummary(ctx, sid, old[0].ID, old[len(old)-1].ID, summary)
	return summary, recent, nil
}
