// Package taskrouting classifies chat/responses requests into a coarse task
// type and rewrites "smart-router/auto"-style virtual model requests to a
// task-specific virtual model, reusing internal/virtualmodels for the actual
// provider/target selection, load balancing, and failover.
package taskrouting

// TaskType is a coarse task category used to pick a cost/quality tier.
type TaskType string

const (
	TaskAgentToolUse    TaskType = "agent_tool_use"
	TaskCode            TaskType = "code"
	TaskTranslation     TaskType = "translation"
	TaskTitleGeneration TaskType = "title_generation"
	TaskBulkLowValue    TaskType = "bulk_low_value"
	TaskCopywriting     TaskType = "copywriting"
	TaskDataExtraction  TaskType = "data_extraction"
	TaskGeneral         TaskType = "general"
)

// Classification is a classifier's decision for one request.
type Classification struct {
	Task   TaskType
	Reason string // matched rule name, for logging/debugging
}

// Message is the minimal chat message shape a classifier needs.
type Message struct {
	Role    string
	Content string
}

// ClassificationInput carries only the signals a classifier needs, kept
// independent of core.ChatRequest so classifiers don't depend on transport types.
type ClassificationInput struct {
	Messages       []Message
	HasToolCalls   bool
	ToolNames      []string
	RequestLabels  []string // existing tagging labels (e.g. "batch") on this request
	RequestedModel string   // the entrypoint alias requested, for logging only
}

// LastUserMessage returns the content of the last message with Role == "user",
// or "" if there is none.
func (in ClassificationInput) LastUserMessage() string {
	for i := len(in.Messages) - 1; i >= 0; i-- {
		if in.Messages[i].Role == "user" {
			return in.Messages[i].Content
		}
	}
	return ""
}
