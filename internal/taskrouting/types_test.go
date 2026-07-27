package taskrouting

import (
	"testing"
)

func TestClassificationInputLastUserMessage(t *testing.T) {
	tests := []struct {
		name     string
		input    ClassificationInput
		expected string
	}{
		{
			name:     "no messages",
			input:    ClassificationInput{Messages: []Message{}},
			expected: "",
		},
		{
			name: "single user message",
			input: ClassificationInput{Messages: []Message{
				{Role: "user", Content: "hello there"},
			}},
			expected: "hello there",
		},
		{
			name: "multiple messages with last as user",
			input: ClassificationInput{Messages: []Message{
				{Role: "assistant", Content: "hi"},
				{Role: "system", Content: "system prompt"},
				{Role: "user", Content: "final question"},
			}},
			expected: "final question",
		},
		{
			name: "multiple messages with last NOT user",
			input: ClassificationInput{Messages: []Message{
				{Role: "user", Content: "my question"},
				{Role: "assistant", Content: "my answer"},
			}},
			expected: "my question",
		},
		{
			name: "multiple users, take last one",
			input: ClassificationInput{Messages: []Message{
				{Role: "user", Content: "first question"},
				{Role: "assistant", Content: "first answer"},
				{Role: "user", Content: "second question"},
				{Role: "assistant", Content: "second answer"},
			}},
			expected: "second question",
		},
		{
			name: "only system and assistant messages",
			input: ClassificationInput{Messages: []Message{
				{Role: "system", Content: "system prompt"},
				{Role: "assistant", Content: "assistant reply"},
			}},
			expected: "",
		},
		{
			name: "user surrounded by assistants",
			input: ClassificationInput{Messages: []Message{
				{Role: "assistant", Content: "first"},
				{Role: "user", Content: "in the middle"},
				{Role: "assistant", Content: "last"},
			}},
			expected: "in the middle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.input.LastUserMessage()
			if got != tt.expected {
				t.Errorf("LastUserMessage() = %q, want %q", got, tt.expected)
			}
		})
	}
}
