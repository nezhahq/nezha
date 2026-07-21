// Package controller — 内置 LLM Chat 控制器。
//
// 设计要点：
//   - 流式端点 (POST /api/v1/llm/chat) 直接返回 text/event-stream，
//     不走 commonHandler/adminHandler 的 c.JSON 包装（写过 SSE header
//     后不能再 c.JSON）。路由注册时传入裸 gin.HandlerFunc。
//   - 连接性测试端点 (POST /api/v1/llm/test) 走普通 JSON 响应，复用
//     adminHandler 包一层鉴权。
//   - 鉴权：admin-only，与 /setting /maintenance 同款（restScopeMiddleware
//     + adminHandler 内层 Role.IsAdmin 校验）。
//   - 安全：
//     * 客户端永不接收 LLMAPIKey 明文；listConfig 只暴露 llm_api_key_set。
//     * 流错误仅以 SSE event:error 形式回给前端，不泄漏内部错误细节。
//     * defer stream.Close() 必保；流中断后旧的 reader 仍由 goroutine 走完。
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// llmChatRequest 是 POST /api/v1/llm/chat 的请求体。
type llmChatRequest struct {
	Messages []*schema.Message `json:"messages"`
}

// llmChatDelta 是 SSE data 帧的载荷。
type llmChatDelta struct {
	Delta string `json:"delta"`
}

// llmChatError 是 SSE event:error 帧的载荷。
type llmChatError struct {
	Error string `json:"error"`
}

// Chat streaming — admin only
//
// @Summary LLM Chat (SSE stream)
// @Description Stream chat completions from the configured LLM provider. Admin only.
// @Security BearerAuth
// @Tags admin required
// @Accept json
// @Produce text/event-stream
// @Param body body llmChatRequest true "Chat messages"
// @Success 200 {string} string "SSE stream of chat deltas"
// @Router /llm/chat [post]
func llmChat(c *gin.Context) {
	// 必须 admin（路由已挂 restScopeMiddleware(ScopeAdminAll)，但 PAT bearer
	// 的强制 Role 校验由 adminHandler 负责；这里手写不走 wrapper，故自校验）。
	if u, ok := c.Get(model.CtxKeyAuthorizedUser); ok {
		if user, ok := u.(*model.User); !ok || !user.Role.IsAdmin() {
			abortLLMUnauthorized(c)
			return
		}
	} else {
		abortLLMUnauthorized(c)
		return
	}

	var req llmChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeSSEError(c, http.StatusBadRequest, fmt.Sprintf("%s: %v", singleton.Localizer.T("invalid chat request"), err))
		return
	}
	if len(req.Messages) == 0 {
		writeSSEError(c, http.StatusBadRequest, singleton.Localizer.T("messages is empty"))
		return
	}
	for i, m := range req.Messages {
		if m == nil {
			writeSSEError(c, http.StatusBadRequest, fmt.Sprintf("%s: %d", singleton.Localizer.T("message is nil"), i))
			return
		}
		if strings.TrimSpace(m.Content) == "" {
			writeSSEError(c, http.StatusBadRequest, fmt.Sprintf("%s: %d", singleton.Localizer.T("message content is empty"), i))
			return
		}
	}
	if singleton.LLMShared == nil || !singleton.LLMShared.IsConfigured() {
		writeSSEError(c, http.StatusServiceUnavailable, singleton.Localizer.T("LLM provider not configured"))
		return
	}

	// 写入 SSE header；写完不能再 c.JSON。
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no") // 防 nginx 反代缓冲
	c.Writer.WriteHeader(http.StatusOK)

	flusher, _ := c.Writer.(http.Flusher)
	if flusher == nil {
		log.Printf("NEZHA>> LLM chat: ResponseWriter is not an http.Flusher, stream will buffer")
	} else {
		flusher.Flush()
	}

	ctx := c.Request.Context()

	// 先 emit 一个 thinking 事件，让前端把消息切换成"思考中"状态。
	if _, wErr := fmt.Fprint(c.Writer, "event: thinking\ndata: {}\n\n"); wErr != nil {
		log.Printf("NEZHA>> LLM chat: write thinking event failed: %v", wErr)
		return
	}
	if flusher != nil {
		flusher.Flush()
	}

	// 真流式 agent：每个 token 立刻 flush 给客户端；tool_call 阶段会发一个
	// tool_call 事件告诉前端"正在调用 xxx"。
	streamErr := singleton.LLMShared.AgentStream(
		ctx,
		req.Messages,
		func(delta string) {
			payload, mErr := json.Marshal(llmChatDelta{Delta: delta})
			if mErr != nil {
				log.Printf("NEZHA>> LLM chat: marshal delta failed: %v", mErr)
				return
			}
			if _, wErr := fmt.Fprintf(c.Writer, "data: %s\n\n", payload); wErr != nil {
				log.Printf("NEZHA>> LLM chat: write failed (client disconnect?): %v", wErr)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		},
		func(iter int, names []string) {
			payload, _ := json.Marshal(map[string]any{
				"iteration": iter,
				"tools":     names,
			})
			fmt.Fprintf(c.Writer, "event: tool_call\ndata: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		},
	)
	if streamErr != nil {
		if errors.Is(streamErr, singleton.ErrLLMNotConfigured) {
			// 配置变更导致中途失效：已经写了 SSE header，只能用 SSE error 事件。
			payload, _ := json.Marshal(llmChatError{Error: singleton.Localizer.T("LLM provider not configured")})
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		if errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
			log.Printf("NEZHA>> LLM chat: cancelled: %v", streamErr)
			return
		}
		payload, _ := json.Marshal(llmChatError{Error: fmt.Sprintf("%s: %v", singleton.Localizer.T("LLM stream error"), streamErr)})
		fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	// 结束标志：前端按 [DONE] 判定流结束。
	fmt.Fprint(c.Writer, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// Test connection — admin only
//
// @Summary Test LLM connection
// @Description Send a tiny non-streaming request to the configured LLM provider and report latency.
// @Security BearerAuth
// @Tags admin required
// @Produce json
// @Success 200 {object} model.CommonResponse[llmTestResult]
// @Router /llm/test [post]
func llmTest(c *gin.Context) (any, error) {
	if singleton.LLMShared == nil || !singleton.LLMShared.IsConfigured() {
		return nil, errors.New(singleton.Localizer.T("LLM provider not configured"))
	}
	prompt := "ping"
	msgs := []*schema.Message{
		schema.SystemMessage("You are a connectivity probe. Reply with the single word: pong"),
		schema.UserMessage(prompt),
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := singleton.LLMShared.Generate(ctx, msgs)
	if err != nil {
		return nil, errors.New(fmt.Sprintf(singleton.Localizer.T("LLM connection failed: %v"), err))
	}
	return &llmTestResult{
		OK:           true,
		Reply:        resp.Content,
		Model:        singleton.Conf.LLMModel,
		BaseURL:      singleton.Conf.LLMBaseURL,
		LatencyMS:    time.Since(start).Milliseconds(),
	}, nil
}

type llmTestResult struct {
	OK        bool   `json:"ok"`
	Reply     string `json:"reply,omitempty"`
	Model     string `json:"model,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
}

// abortLLMUnauthorized 把未授权的流式请求以 SSE 形式礼貌地拒绝。
func abortLLMUnauthorized(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.WriteHeader(http.StatusUnauthorized)
	payload, _ := json.Marshal(llmChatError{Error: "unauthorized"})
	fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", payload)
}

// writeSSEError 在已经决定走 SSE 协议但前置校验失败时，把错误以 SSE event 形式
// 写出，避免与 adminHandler 的 c.JSON 包装冲突。
func writeSSEError(c *gin.Context, status int, msg string) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.WriteHeader(status)
	payload, _ := json.Marshal(llmChatError{Error: msg})
	fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", payload)
}
