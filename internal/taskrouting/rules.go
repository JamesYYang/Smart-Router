package taskrouting

import (
	"context"
	"strings"
)

// rule is one ordered step in RuleClassifier's chain. Match reports whether
// this rule applies to input; the first matching rule wins.
type rule struct {
	name  string
	task  TaskType
	match func(ClassificationInput) bool
}

// RuleClassifier is the P0 MVP Classifier: a fixed, ordered chain of
// keyword/structure heuristics, falling back to TaskGeneral when nothing
// matches. Pure string/regex matching — no external calls, microsecond-level
// cost per request.
type RuleClassifier struct {
	rules []rule
}

// NewRuleClassifier builds the default P0 rule chain (see
// docs/superpowers/specs/2026-07-27-taskrouting-design.md §3.2).
func NewRuleClassifier() *RuleClassifier {
	return &RuleClassifier{rules: defaultRules}
}

func (c *RuleClassifier) Classify(_ context.Context, input ClassificationInput) (Classification, error) {
	for _, r := range c.rules {
		if r.match(input) {
			return Classification{Task: r.task, Reason: r.name}, nil
		}
	}
	return Classification{Task: TaskGeneral, Reason: "no_rule_matched"}, nil
}

var defaultRules = []rule{
	{name: "agent_tool_use", task: TaskAgentToolUse, match: matchAgentToolUse},
	{name: "code", task: TaskCode, match: matchCode},
	{name: "translation", task: TaskTranslation, match: matchTranslation},
	{name: "title_generation", task: TaskTitleGeneration, match: matchTitleGeneration},
	{name: "bulk_low_value", task: TaskBulkLowValue, match: matchBulkLowValue},
	{name: "copywriting", task: TaskCopywriting, match: matchCopywriting},
	{name: "data_extraction", task: TaskDataExtraction, match: matchDataExtraction},
}

func matchAgentToolUse(in ClassificationInput) bool {
	return in.HasToolCalls
}

var codeKeywords = []string{"func ", "def ", "class ", "import ", "package "}

func matchCode(in ClassificationInput) bool {
	msg := in.LastUserMessage()
	if strings.Contains(msg, "```") {
		return true
	}
	lower := strings.ToLower(msg)
	hits := 0
	for _, kw := range codeKeywords {
		if strings.Contains(lower, kw) {
			hits++
		}
	}
	return hits >= 2
}

var translationPhrases = []string{"翻译成", "翻译为", "译为", "translate to", "translate into"}

func matchTranslation(in ClassificationInput) bool {
	lower := strings.ToLower(in.LastUserMessage())
	for _, phrase := range translationPhrases {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			return true
		}
	}
	return false
}

func matchTitleGeneration(in ClassificationInput) bool {
	msg := in.LastUserMessage()
	if len([]rune(msg)) >= 50 {
		return false
	}
	return strings.Contains(msg, "标题") || strings.Contains(strings.ToLower(msg), "title")
}

func matchBulkLowValue(in ClassificationInput) bool {
	for _, label := range in.RequestLabels {
		if strings.EqualFold(strings.TrimSpace(label), "batch") {
			return true
		}
	}
	return false
}

var copywritingKeywords = []string{"写一篇", "文案", "slogan", "营销"}

func matchCopywriting(in ClassificationInput) bool {
	msg := in.LastUserMessage()
	lower := strings.ToLower(msg)
	for _, kw := range copywritingKeywords {
		if strings.Contains(msg, kw) || strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

var dataExtractionKeywords = []string{"提取", "抽取", "extract", "json schema", "结构化输出"}

func matchDataExtraction(in ClassificationInput) bool {
	msg := in.LastUserMessage()
	lower := strings.ToLower(msg)
	for _, kw := range dataExtractionKeywords {
		if strings.Contains(msg, kw) || strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
