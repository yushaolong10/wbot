package agent

import (
	"github.com/wbot-dev/wbot/internal/domain"
	"strings"
)

type Verification struct {
	Passed            bool        `json:"passed"`
	Criteria          []Criterion `json:"criteria"`
	RecommendedAction string      `json:"recommended_action"`
}
type Criterion struct {
	Criterion string `json:"criterion"`
	Passed    bool   `json:"passed"`
	Reason    string `json:"reason"`
}

func evaluate(content string, msgs []domain.Message) Verification {
	v := Verification{Passed: true, RecommendedAction: "complete"}
	nonempty := strings.TrimSpace(content) != ""
	v.Criteria = append(v.Criteria, Criterion{"模型返回非空交付结果", nonempty, "结果必须包含可交付内容"})
	unresolved := false
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "tool" {
			unresolved = strings.Contains(msgs[i].Content, `"Status":"error"`) || strings.Contains(msgs[i].Content, `"status":"error"`)
			break
		}
	}
	v.Criteria = append(v.Criteria, Criterion{"不存在未处理的工具错误", !unresolved, "最后一次任务尝试不能留下工具错误"})
	v.Passed = nonempty && !unresolved
	if !v.Passed {
		v.RecommendedAction = "replan"
	}
	return v
}
