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
	toolPassed, toolReason, err := evaluateToolResults(msgs)
	if err != nil {
		toolPassed = false
		toolReason = err.Error()
	}
	v.Criteria = append(v.Criteria, Criterion{"不存在未处理的工具错误", toolPassed, toolReason})
	v.Passed = nonempty && toolPassed
	if !v.Passed {
		v.RecommendedAction = "replan"
	}
	return v
}
