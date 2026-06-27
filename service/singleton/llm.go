package singleton

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino-ext/components/model/openai"
)

// ErrLLMNotConfigured LLM 未配置或被禁用。
var ErrLLMNotConfigured = errors.New("LLM provider not configured")

// maxAgentIterations 限制单次请求的最大 tool-call 循环次数，防止模型陷入死循环。
const maxAgentIterations = 8

// LLMClass 持有当前的 *openai.ChatModel 与上一份配置快照；Reload 时按新
// 配置重建 ChatModel，配置变更时其他在跑的流不受影响（它们继续持有旧
// ChatModel 的 stream reader，新请求走新 client）。
type LLMClass struct {
	mu      sync.RWMutex
	chat    *openai.ChatModel
	cfg     llmSnapshot
	tools   *LLMRegistry
	toolMap map[string]tool.InvokableTool
}

// DeltaFunc 接收模型流式输出的单个 delta 字符串。
// 实现端应该立即 flush 给客户端，不要 buffer。
type DeltaFunc func(delta string)

// ToolCallFunc 当模型开始 / 完成一轮 tool 调用时触发，可用于 UI 提示。
// iteration 从 1 开始；每次进入新一轮 tool 调用时递增。
type ToolCallFunc func(iteration int, toolNames []string)

type llmSnapshot struct {
	enableLLM    bool
	baseURL      string
	model        string
	apiKey       string
	systemPrompt string
	maxTokens    int
	temperature  float32
}

// NewLLMClass 构造 LLMClass；不在此处加载配置，由 Reload 按当前 Conf 初始化。
func NewLLMClass() *LLMClass {
	r := NewLLMRegistry()
	toolMap := make(map[string]tool.InvokableTool, len(r.Tools()))
	for _, t := range r.Tools() {
		info, _ := t.Info(context.Background())
		toolMap[info.Name] = t
	}
	return &LLMClass{
		tools:   r,
		toolMap: toolMap,
	}
}

// Tools 返回当前注册的 tool 列表（用于前端展示工具清单或调试）。
func (l *LLMClass) Tools() []tool.InvokableTool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.tools == nil {
		return nil
	}
	return l.tools.Tools()
}

// Reload 按当前 singleton.Conf 重建 ChatModel；enable=false 时清空。
// 配置未变（baseURL/model/apiKey/温度/最大token 一致）时直接返回 nil 跳过重建。
func (l *LLMClass) Reload() error {
	next := llmSnapshot{
		enableLLM:    Conf.EnableLLM,
		baseURL:      Conf.LLMBaseURL,
		model:        Conf.LLMModel,
		apiKey:       Conf.LLMAPIKey,
		systemPrompt: Conf.LLMSystemPrompt,
		maxTokens:    Conf.LLMMaxTokens,
		temperature:  Conf.LLMTemperature,
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if next.enableLLM && next.apiKey != "" && next.model != "" {
		// 配置没变就不重建，避免无意义的 client 重建。
		if l.chat != nil && l.cfg == next {
			return nil
		}
		cm, err := openai.NewChatModel(context.Background(), &openai.ChatModelConfig{
			APIKey:              next.apiKey,
			Model:               next.model,
			BaseURL:             next.baseURL,
			Temperature:         float32Ptr(next.temperature),
			MaxCompletionTokens: intPtr(next.maxTokens),
		})
		if err != nil {
			// 构造失败：保留旧 client 以让旧流仍能结束，但不接受新请求。
			log.Printf("NEZHA>> LLM ChatModel rebuild failed: %v", err)
			l.chat = nil
			l.cfg = next
			return err
		}
		l.chat = cm
		l.cfg = next
		return nil
	}

	// 未配置：清空。
	l.chat = nil
	l.cfg = next
	return nil
}

// IsConfigured 返回是否启用且 API key / model 都已配置。
func (l *LLMClass) IsConfigured() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.chat != nil
}

// Generate 非流式调用；用于连通性测试与未来不需要流式的端点。
func (l *LLMClass) Generate(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	l.mu.RLock()
	cm := l.chat
	sysPrompt := l.cfg.systemPrompt
	l.mu.RUnlock()

	if cm == nil {
		return nil, ErrLLMNotConfigured
	}
	full := prependSystem(sysPrompt, msgs)
	return cm.Generate(ctx, full)
}

// Stream 流式调用；调用方负责对返回的 reader 调 Close()。
// 推荐用 defer reader.Close()，并用 errors.Is(err, io.EOF) 判结束。
func (l *LLMClass) Stream(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	l.mu.RLock()
	cm := l.chat
	sysPrompt := l.cfg.systemPrompt
	l.mu.RUnlock()

	if cm == nil {
		return nil, ErrLLMNotConfigured
	}
	full := prependSystem(sysPrompt, msgs)
	return cm.Stream(ctx, full)
}

// AgentRun 是带 tool 调用的 chat agent 入口：模型返回 tool_calls 时执行对应
// 工具并把结果回填到 messages 里，循环直到模型给出最终 assistant 消息。
//
// 与 Stream 不同，AgentRun 返回单条最终消息（不流式）；适合"先思考、再回答"
// 的场景。如果工具链很长，可以监听 ctx.Done() 提前退出。
//
// 工具执行失败的错误信息会被包装成 tool message 返回给模型，让模型自己
// 决定如何应对（重试 / 改用其它工具 / 告诉用户失败原因）。
func (l *LLMClass) AgentRun(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	l.mu.RLock()
	cm := l.chat
	sysPrompt := l.cfg.systemPrompt
	tools := l.tools
	l.mu.RUnlock()

	if cm == nil {
		return nil, ErrLLMNotConfigured
	}

	// 准备带 tools 的 ChatModel 变体（每个请求独立，不污染基础 client）。
	executor, err := withTools(cm, tools)
	if err != nil {
		return nil, err
	}

	full := prependSystem(sysPrompt, msgs)
	for iter := 0; iter < maxAgentIterations; iter++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		resp, err := executor.Generate(ctx, full)
		if err != nil {
			return nil, err
		}
		if resp == nil {
			return nil, errors.New("LLM returned nil message")
		}
		if len(resp.ToolCalls) == 0 {
			// 最终回答：清掉 ToolCalls，只保留 Content。
			final := *resp
			final.ToolCalls = nil
			return &final, nil
		}

		// 把 assistant 消息（含 ToolCalls）追加进 history，模型需要看到这个
		// 才知道下一条 tool message 是对哪个 tool_call 的响应。
		full = append(full, resp)

		// 顺序执行每个 tool call。
		for _, tc := range resp.ToolCalls {
			if tc.Function.Name == "" {
				continue
			}
			result, execErr := executeTool(tools, tc.Function.Name, tc.Function.Arguments)
			if execErr != nil {
				result = "tool execution error: " + execErr.Error()
				log.Printf("NEZHA>> tool %q exec failed: %v", tc.Function.Name, execErr)
			}
			full = append(full, schema.ToolMessage(result, tc.ID, schema.WithToolName(tc.Function.Name)))
		}
	}
	return nil, errors.New("agent loop exceeded max iterations")
}

// AgentStream 是真正的流式 agent 入口：
//   - 用 Stream() 替代 Generate()，每个 token 到达立刻调用 onDelta；
//   - 如果模型返回 tool_calls（tool-calling 模型常见），在 stream 结束后
//     执行工具、把结果回填，再开新一轮 Stream 直到给出最终 assistant 消息；
//   - onToolCall 在进入新一轮 tool 阶段时触发，可用于前端显示 "正在调用 xxx"。
//
// 与 AgentRun 区别：AgentRun 是非流式的整批返回，AgentStream 是真流式
// 输出（每个 token 触发 onDelta）。
func (l *LLMClass) AgentStream(
	ctx context.Context,
	msgs []*schema.Message,
	onDelta DeltaFunc,
	onToolCall ToolCallFunc,
) error {
	if onDelta == nil {
		onDelta = func(string) {}
	}

	l.mu.RLock()
	cm := l.chat
	sysPrompt := l.cfg.systemPrompt
	tools := l.tools
	l.mu.RUnlock()

	if cm == nil {
		return ErrLLMNotConfigured
	}

	executor, err := withTools(cm, tools)
	if err != nil {
		return err
	}

	full := prependSystem(sysPrompt, msgs)
	for iter := 0; iter < maxAgentIterations; iter++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		stream, err := executor.Stream(ctx, full)
		if err != nil {
			return err
		}

		// 累积本轮的 text content + tool_calls。
		textAccum := ""
		// toolCalls 按 Index 合并；Index 为 nil 时按出现顺序回填。
		type pendingTC struct {
			id        string
			typ       string
			name      string
			arguments string
		}
		toolCalls := make(map[int]pendingTC)
		nextIdx := 0

		for {
			if err := ctx.Err(); err != nil {
				stream.Close()
				return err
			}
			chunk, recvErr := stream.Recv()
			if recvErr != nil {
				if errors.Is(recvErr, io.EOF) {
					break
				}
				stream.Close()
				return recvErr
			}
			if chunk == nil {
				continue
			}
			if chunk.Content != "" {
				textAccum += chunk.Content
				onDelta(chunk.Content)
			}
			for i := range chunk.ToolCalls {
				tc := &chunk.ToolCalls[i]
				if tc == nil {
					continue
				}
				var idx int
				if tc.Index != nil {
					idx = *tc.Index
				} else {
					idx = nextIdx
					nextIdx++
				}
				entry := toolCalls[idx]
				if tc.ID != "" {
					entry.id = tc.ID
				}
				if tc.Type != "" {
					entry.typ = tc.Type
				}
				if tc.Function.Name != "" {
					entry.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					entry.arguments += tc.Function.Arguments
				}
				toolCalls[idx] = entry
			}
		}
		stream.Close()

		if len(toolCalls) == 0 {
			// 没有 tool call，本轮就是最终回答。
			return nil
		}

		// 把本轮的 assistant 消息（含 tool_calls）追加进 history；下一轮要把
		// 这些信息一起发给模型，否则它不知道 tool_call id 对应哪条 tool message。
		//
		// toolCalls 是 map，无序；按 Index 升序转成 slice，保持稳定性。
		assembled := make([]schema.ToolCall, 0, len(toolCalls))
		names := make([]string, 0, len(toolCalls))
		// 先收集 names 给 onToolCall 用。
		for i := 0; i < len(toolCalls); i++ {
			if e, ok := toolCalls[i]; ok {
				names = append(names, e.name)
			}
		}
		if onToolCall != nil {
			onToolCall(iter+1, names)
		}
		for i := 0; i < len(toolCalls); i++ {
			e, ok := toolCalls[i]
			if !ok {
				continue
			}
			idx := i
			assembled = append(assembled, schema.ToolCall{
				Index: &idx,
				ID:    e.id,
				Type:  e.typ,
				Function: schema.FunctionCall{
					Name:      e.name,
					Arguments: e.arguments,
				},
			})
		}
		assistantMsg := &schema.Message{
			Role:      schema.Assistant,
			Content:   textAccum,
			ToolCalls: assembled,
		}
		full = append(full, assistantMsg)

		// 同步执行每个 tool call（按 Index 顺序）。
		for i := 0; i < len(toolCalls); i++ {
			e, ok := toolCalls[i]
			if !ok || e.name == "" {
				continue
			}
			result, execErr := executeTool(tools, e.name, e.arguments)
			if execErr != nil {
				result = "tool execution error: " + execErr.Error()
				log.Printf("NEZHA>> tool %q exec failed: %v", e.name, execErr)
			}
			full = append(full, schema.ToolMessage(result, e.id, schema.WithToolName(e.name)))
		}
		// 进入下一轮 Stream；继续直到不再有 tool_calls。
	}
	return errors.New("agent loop exceeded max iterations")
}

// withTools 给 ChatModel 加上 tool 集合；如果 registry 为空或没有 tool，
// 返回原始 ChatModel。失败时返回错误（schema 不合法等）。
func withTools(cm *openai.ChatModel, r *LLMRegistry) (model.ToolCallingChatModel, error) {
	if r == nil || len(r.Tools()) == 0 {
		return cm, nil
	}
	infos := make([]*schema.ToolInfo, 0, len(r.Tools()))
	for _, t := range r.Tools() {
		info, err := t.Info(context.Background())
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return cm.WithTools(infos)
}

// executeTool 在 registry 中查找工具并执行；结果以 JSON 字符串返回（tool message 的 content）。
func executeTool(r *LLMRegistry, name, argsJSON string) (string, error) {
	t, ok := lookupTool(r, name)
	if !ok {
		return "", errors.New("tool not found: " + name)
	}
	return t.InvokableRun(context.Background(), argsJSON)
}

func lookupTool(r *LLMRegistry, name string) (tool.InvokableTool, bool) {
	for _, t := range r.Tools() {
		info, err := t.Info(context.Background())
		if err != nil {
			continue
		}
		if info.Name == name {
			return t, true
		}
	}
	return nil, false
}

// AsJSONLines 是给前端调试 / 日志使用的辅助函数：把任意结构体序列化成
// 带缩进的 JSON（普通 json.MarshalIndent 就行，但给个独立名字方便未来
// 替换为更紧凑的输出）。
func AsJSONLines(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

func prependSystem(sys string, msgs []*schema.Message) []*schema.Message {
	if sys == "" {
		return msgs
	}
	out := make([]*schema.Message, 0, len(msgs)+1)
	out = append(out, schema.SystemMessage(sys))
	out = append(out, msgs...)
	return out
}

func float32Ptr(v float32) *float32 {
	if v == 0 {
		return nil
	}
	return &v
}

func intPtr(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}
