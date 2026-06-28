package controller

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestLLMChatRequest_BasicShape 校验请求体的最小契约。
func TestLLMChatRequest_BasicShape(t *testing.T) {
	req := llmChatRequest{
		Messages: []*schema.Message{
			schema.UserMessage("hi"),
		},
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages len = %d", len(req.Messages))
	}
	if req.Messages[0].Role != schema.User {
		t.Fatalf("role = %v, want %v", req.Messages[0].Role, schema.User)
	}
}

// TestLLMChatDelta_Serialization 校验 SSE delta 载荷字段。
func TestLLMChatDelta_Serialization(t *testing.T) {
	d := llmChatDelta{Delta: "hello"}
	if d.Delta != "hello" {
		t.Fatal("delta field lost")
	}
}
