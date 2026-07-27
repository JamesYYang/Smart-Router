package taskrouting

import (
	"context"
	"testing"
)

func classify(t *testing.T, in ClassificationInput) Classification {
	t.Helper()
	c := NewRuleClassifier()
	got, err := c.Classify(context.Background(), in)
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil", err)
	}
	return got
}

func TestRuleClassifier_AgentToolUse(t *testing.T) {
	got := classify(t, ClassificationInput{HasToolCalls: true})
	if got.Task != TaskAgentToolUse {
		t.Fatalf("Task = %q, want %q", got.Task, TaskAgentToolUse)
	}
}

func TestRuleClassifier_CodeFence(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "fix this: ```go\nfunc main() {}\n```"},
	}})
	if got.Task != TaskCode {
		t.Fatalf("Task = %q, want %q", got.Task, TaskCode)
	}
}

func TestRuleClassifier_CodeKeywords(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "please write a func that uses import os and returns"},
	}})
	if got.Task != TaskCode {
		t.Fatalf("Task = %q, want %q", got.Task, TaskCode)
	}
}

func TestRuleClassifier_Translation(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "把这段话翻译成英文：你好"},
	}})
	if got.Task != TaskTranslation {
		t.Fatalf("Task = %q, want %q", got.Task, TaskTranslation)
	}
}

func TestRuleClassifier_TitleGeneration(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "给这篇文章起个标题"},
	}})
	if got.Task != TaskTitleGeneration {
		t.Fatalf("Task = %q, want %q", got.Task, TaskTitleGeneration)
	}
}

func TestRuleClassifier_BulkLowValue(t *testing.T) {
	got := classify(t, ClassificationInput{
		Messages:      []Message{{Role: "user", Content: "summarize this"}},
		RequestLabels: []string{"batch"},
	})
	if got.Task != TaskBulkLowValue {
		t.Fatalf("Task = %q, want %q", got.Task, TaskBulkLowValue)
	}
}

func TestRuleClassifier_Copywriting(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "帮我写一篇产品文案"},
	}})
	if got.Task != TaskCopywriting {
		t.Fatalf("Task = %q, want %q", got.Task, TaskCopywriting)
	}
}

func TestRuleClassifier_DataExtraction(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "从这段文本中提取姓名和日期，按 JSON schema 输出"},
	}})
	if got.Task != TaskDataExtraction {
		t.Fatalf("Task = %q, want %q", got.Task, TaskDataExtraction)
	}
}

func TestRuleClassifier_FallsBackToGeneral(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "What's the weather like today?"},
	}})
	if got.Task != TaskGeneral {
		t.Fatalf("Task = %q, want %q", got.Task, TaskGeneral)
	}
	if got.Reason != "no_rule_matched" {
		t.Fatalf("Reason = %q, want %q", got.Reason, "no_rule_matched")
	}
}
