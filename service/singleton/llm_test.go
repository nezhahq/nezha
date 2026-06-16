package singleton

import (
	"testing"

	"github.com/nezhahq/nezha/model"
)

// TestLLMClass_NotConfiguredWhenFieldsEmpty 验证缺关键字段时 IsConfigured 返回 false。
func TestLLMClass_NotConfiguredWhenFieldsEmpty(t *testing.T) {
	// 备份 & 恢复全局 Conf。
	orig := Conf
	defer func() { Conf = orig }()
	Conf = &ConfigClass{Config: &model.Config{}}

	l := NewLLMClass()
	// 缺 enable / api key / model 时不构造 client。
	if l.IsConfigured() {
		t.Fatal("expected IsConfigured=false when EnableLLM=false")
	}

	Conf.EnableLLM = true
	if l.IsConfigured() {
		t.Fatal("expected IsConfigured=false when APIKey empty")
	}

	Conf.LLMAPIKey = "sk-test"
	if l.IsConfigured() {
		t.Fatal("expected IsConfigured=false when Model empty")
	}

	Conf.LLMModel = "gpt-4o-mini"
	if err := l.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !l.IsConfigured() {
		t.Fatal("expected IsConfigured=true after Reload with all fields set")
	}
}

// TestLLMClass_ReloadIdempotent 验证重复 Reload 同一份配置不会重建 client。
// 不直接验证内部指针，而是验证 IsConfigured 维持稳定。
func TestLLMClass_ReloadIdempotent(t *testing.T) {
	orig := Conf
	defer func() { Conf = orig }()
	Conf = &ConfigClass{
		Config: &model.Config{
			EnableLLM:  true,
			LLMAPIKey:  "sk-test",
			LLMModel:   "gpt-4o-mini",
			LLMBaseURL: "https://api.openai.com/v1",
		},
	}

	l := NewLLMClass()
	if err := l.Reload(); err != nil {
		t.Fatalf("first Reload: %v", err)
	}
	if !l.IsConfigured() {
		t.Fatal("expected IsConfigured=true after first Reload")
	}
	if err := l.Reload(); err != nil {
		t.Fatalf("second Reload: %v", err)
	}
	if !l.IsConfigured() {
		t.Fatal("expected IsConfigured=true after second Reload")
	}
}

// TestLLMClass_DisableClearsClient 验证关闭后 IsConfigured 变 false。
func TestLLMClass_DisableClearsClient(t *testing.T) {
	orig := Conf
	defer func() { Conf = orig }()
	Conf = &ConfigClass{
		Config: &model.Config{
			EnableLLM: true,
			LLMAPIKey: "sk-test",
			LLMModel:  "gpt-4o-mini",
		},
	}

	l := NewLLMClass()
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	if !l.IsConfigured() {
		t.Fatal("expected IsConfigured=true")
	}

	Conf.EnableLLM = false
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	if l.IsConfigured() {
		t.Fatal("expected IsConfigured=false after disable")
	}
}
