package model

import (
	"encoding/json"
	"sigs.k8s.io/yaml"
	"strings"
	"testing"
)

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func yamlMarshal(v any) (string, error) {
	b, err := yaml.Marshal(v)
	return string(b), err
}

func yamlUnmarshal(data []byte, v any) error {
	return yaml.Unmarshal(data, v)
}

// TestConfig_LLMAPIKeyNotSerialized 验证 LLMAPIKey 永远不进入 JSON/YAML 序列化。
// 这是 listConfig 不回显明文 API key 的关键保证。
func TestConfig_LLMAPIKeyNotSerialized(t *testing.T) {
	c := &Config{
		LLMAPIKey: "sk-secret-1234567890",
	}

	// JSON：应不含明文 key。
	if got, err := jsonMarshal(c); err != nil {
		t.Fatalf("marshal: %v", err)
	} else if strings.Contains(got, "sk-secret-1234567890") {
		t.Fatalf("LLMAPIKey leaked into JSON: %s", got)
	}

	// YAML：同理。
	if got, err := yamlMarshal(c); err != nil {
		t.Fatalf("marshal: %v", err)
	} else if strings.Contains(got, "sk-secret-1234567890") {
		t.Fatalf("LLMAPIKey leaked into YAML: %s", got)
	}
}

// TestConfig_LLMFieldsRoundTrip 验证 LLM 字段从 YAML 读回时维持一致。
func TestConfig_LLMFieldsRoundTrip(t *testing.T) {
	c := &Config{
		EnableLLM:       true,
		LLMBaseURL:      "https://api.openai.com/v1",
		LLMModel:        "gpt-4o-mini",
		LLMAPIKey:       "sk-test",
		LLMSystemPrompt: "you are an assistant",
		LLMMaxTokens:    2048,
		LLMTemperature:  0.5,
	}
	c.SetLLMAPIKey("sk-test")
	if !c.LLMAPIKeySet() {
		t.Fatalf("LLMAPIKeySet should be true after SetLLMAPIKey")
	}

	// 重新解析回来。
	out, err := yamlMarshal(c)
	if err != nil {
		t.Fatal(err)
	}
	// YAML 不含 llm_api_key（已通过上面 TestConfig_LLMAPIKeyNotSerialized 验证）；
	// 解析时 llm_api_key 字段为空。
	c2 := &Config{}
	if err := yamlUnmarshal([]byte(out), c2); err != nil {
		t.Fatal(err)
	}
	if !c2.EnableLLM {
		t.Errorf("EnableLLM lost")
	}
	if c2.LLMBaseURL != "https://api.openai.com/v1" {
		t.Errorf("LLMBaseURL got %q", c2.LLMBaseURL)
	}
	if c2.LLMModel != "gpt-4o-mini" {
		t.Errorf("LLMModel got %q", c2.LLMModel)
	}
	if c2.LLMMaxTokens != 2048 {
		t.Errorf("LLMMaxTokens got %d", c2.LLMMaxTokens)
	}
}

// TestConfig_SetLLMAPIKey 验证 SetLLMAPIKey 同步更新 llmAPIKeySet 镜像。
func TestConfig_SetLLMAPIKey(t *testing.T) {
	c := &Config{}
	if c.LLMAPIKeySet() {
		t.Fatal("freshly constructed config should report LLMAPIKeySet=false")
	}
	c.SetLLMAPIKey("sk-abc")
	if !c.LLMAPIKeySet() {
		t.Fatal("after SetLLMAPIKey(\"sk-abc\"), LLMAPIKeySet should be true")
	}
	if c.LLMAPIKey != "sk-abc" {
		t.Fatalf("LLMAPIKey = %q, want %q", c.LLMAPIKey, "sk-abc")
	}
	c.SetLLMAPIKey("")
	if c.LLMAPIKeySet() {
		t.Fatal("after SetLLMAPIKey(\"\"), LLMAPIKeySet should be false")
	}
}
