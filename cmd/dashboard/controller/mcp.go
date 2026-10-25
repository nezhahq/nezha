// Package controller — MCP (Model Context Protocol) server.
//
// 落地约束：
//   - 仅支持 Streamable HTTP transport 的 POST 半边（请求-响应、无 SSE）。
//     首版面向 LLM 工具调用，不需要 server→client 主动推送。后续要做 GET SSE
//     长连接（resource subscription）时再补；客户端兼容 fallback 到普通 POST。
//   - JSON-RPC 2.0 编解码内嵌于本文件，未引入第三方 MCP SDK：MCP 协议表面足够小
//     （initialize / tools/list / tools/call），自实现可控、零额外依赖。
//   - 双层鉴权：闸 1（用户对 server 的所有权）由各 tool handler 调
//     singleton.ServerShared.Get + Server.HasPermission；闸 2（PAT scope）由
//     mcpTool.RequiredScope 在 dispatch 之前过滤。
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
)

// --- JSON-RPC 2.0 wire types ---

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

const (
	// JSON-RPC 标准错误码
	rpcErrParse          = -32700
	rpcErrInvalidRequest = -32600
	rpcErrMethodNotFound = -32601
	rpcErrInvalidParams  = -32602
	rpcErrInternal       = -32603
	// MCP 自定义错误码（>= -32000 高位段）
	rpcErrUnauthorized = -32001
	rpcErrForbidden    = -32002
)

// mcpJSONRPCMaxBodyBytes caps the JSON-RPC envelope size at the dashboard
// edge. Real fs.write base64 content goes through fs.transfer (capped
// separately by model.MCPFsTransferMaxSize) so tools/call params here are
// always small. The cap is intentionally generous (8 MiB) to allow
// per-request batched arguments while making OOM-via-decode impossible.
const mcpJSONRPCMaxBodyBytes = 8 * 1024 * 1024

// --- MCP types ---

// mcpServerInfo MCP initialize 响应的 serverInfo 字段。
type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpInitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      mcpServerInfo  `json:"serverInfo"`
}

// mcpToolDescriptor 是 tools/list 返回的单条 tool 描述。
type mcpToolDescriptor struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  map[string]any  `json:"inputSchema"`
	OutputSchema map[string]any  `json:"outputSchema,omitempty"`
	Annotations  map[string]bool `json:"annotations,omitempty"`
}

// mcpToolsListResult tools/list 响应。
type mcpToolsListResult struct {
	Tools []mcpToolDescriptor `json:"tools"`
}

// mcpContent 是 tools/call 响应里 content[] 的元素。
// 仅实现 text 类型；嵌入对象的结构化数据放在外层 structuredContent。
type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// mcpToolCallResult tools/call 响应。
type mcpToolCallResult struct {
	Content           []mcpContent `json:"content"`
	StructuredContent any          `json:"structuredContent,omitempty"`
	IsError           bool         `json:"isError,omitempty"`
}

// --- tool 注册框架 ---

// mcpToolHandler 实际业务逻辑：拿到 raw params + gin ctx，返回任意可序列化结构。
type mcpToolHandler func(c *gin.Context, params json.RawMessage) (any, error)

// mcpTool 是注册表里的单元：声明 + scope 要求 + 处理函数。
type mcpTool struct {
	Name          string
	Description   string
	InputSchema   map[string]any
	OutputSchema  map[string]any // 可选；声明 structuredContent 形状，供严格客户端校验
	RequiredScope string         // 闸 2 入口；空字符串 = 任意 PAT 都能调（如 meta.whoami）
	Handler       mcpToolHandler
}

var (
	mcpToolsMu sync.RWMutex
	mcpTools   = map[string]*mcpTool{}
)

// registerMCPTool 把一个 tool 加进全局注册表。建议各 tool 文件在 init() 里调用。
func registerMCPTool(t *mcpTool) {
	if t == nil || t.Name == "" || t.Handler == nil {
		panic("registerMCPTool: invalid tool")
	}
	mcpToolsMu.Lock()
	defer mcpToolsMu.Unlock()
	if _, dup := mcpTools[t.Name]; dup {
		panic("registerMCPTool: duplicate name " + t.Name)
	}
	mcpTools[t.Name] = t
}

// listRegisteredMCPTools 拷贝一份当前注册表（按名字稳定排序逻辑放在调用方）。
func listRegisteredMCPTools() []*mcpTool {
	mcpToolsMu.RLock()
	defer mcpToolsMu.RUnlock()
	out := make([]*mcpTool, 0, len(mcpTools))
	for _, t := range mcpTools {
		out = append(out, t)
	}
	return out
}

// --- 入口 handler ---

// mcpEndpoint 处理 POST /mcp。
// 鉴权：上游 apiTokenAuthMiddleware 已经把 PAT 解析到 CtxKeyAuthorizedUser，
// OAuth 也解析成同一权限身份；不接受裸 JWT，避免浏览器误触。
func mcpEndpoint(c *gin.Context) {
	if singleton.Conf == nil || !singleton.Conf.MCPEnabled() {
		writeJSONRPCError(c, nil, rpcErrForbidden, "MCP is disabled by the dashboard administrator")
		return
	}
	tok := APITokenFromContext(c)
	if tok == nil {
		mcpOAuthSetChallenge(c)
		// 同时返回 HTTP 401 + JSON-RPC error：标准 MCP HTTP client 依赖
		// HTTP 401 触发 auth 重试/OAuth discovery；JSON-RPC body 保留旧字段
		// 不打破 ScopeDenied 类内部断言。
		writeJSONRPCErrorWithStatus(c, nil, rpcErrUnauthorized, "missing or invalid API token", http.StatusUnauthorized)
		return
	}

	// MaxBytesReader 必须夹在 PAT 校验通过后、ShouldBindJSON 之前——
	// 校验前限流可能让攻击者用伪造 token 触发 audit；校验后限流既挡住合法
	// PAT 的 OOM，又不会让匿名请求走到 audit 路径。
	if c.Request != nil && c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, mcpJSONRPCMaxBodyBytes)
	}

	// Consume the per-token budget before validating the request so malformed
	// envelopes and malformed tools/call params cannot flood the dashboard
	// without counting against the limiter. The outcome is applied after the
	// method is known so tools/call still surfaces the rate limit as a tool
	// error rather than a transport-level error.
	rateLimited := !mcpRateLimiterShared.Allow(tok.ID)

	var req jsonRPCRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		if errors.Is(err, errors.New("http: request body too large")) || strings.Contains(err.Error(), "http: request body too large") {
			writeJSONRPCErrorWithStatus(c, nil, rpcErrInvalidRequest, "request body exceeds MCP envelope size limit", http.StatusRequestEntityTooLarge)
			return
		}
		// 限流优先：method 无从得知时，over-budget 请求即便 body 畸形也必须
		// 走 429，否则攻击者能用畸形 body 在不计入限额的情况下持续刷 parse error。
		if rateLimited {
			writeJSONRPCErrorWithStatus(c, nil, rpcErrForbidden, "rate limit exceeded for this token", http.StatusTooManyRequests)
			return
		}
		writeJSONRPCError(c, nil, rpcErrParse, "invalid json-rpc envelope: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		if rateLimited {
			writeJSONRPCErrorWithStatus(c, req.ID, rpcErrForbidden, "rate limit exceeded for this token", http.StatusTooManyRequests)
			return
		}
		writeJSONRPCError(c, req.ID, rpcErrInvalidRequest, "invalid json-rpc envelope")
		return
	}

	if rateLimited {
		if req.Method == "tools/call" {
			writeToolCallError(c, req.ID, model.MCPOutcomeRateLimited, "rate limit exceeded for this token")
			return
		}
		writeJSONRPCErrorWithStatus(c, req.ID, rpcErrForbidden, "rate limit exceeded for this token", http.StatusTooManyRequests)
		return
	}

	switch req.Method {
	case "initialize":
		writeJSONRPCResult(c, req.ID, mcpInitializeResult{
			ProtocolVersion: "2024-11-05",
			Capabilities: map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			ServerInfo: mcpServerInfo{
				Name:    "nezha-mcp",
				Version: singleton.Version,
			},
		})
	case "notifications/initialized", "ping":
		// 客户端通知或心跳；JSON-RPC 通知没有 id，但 ping 有 id 时返回空 result
		if len(req.ID) > 0 && string(req.ID) != "null" {
			writeJSONRPCResult(c, req.ID, struct{}{})
			return
		}
		c.Status(http.StatusAccepted)
	case "tools/list":
		writeJSONRPCResult(c, req.ID, mcpToolsListResult{
			Tools: buildToolDescriptors(),
		})
	case "tools/call":
		handleToolsCall(c, &req, tok)
	default:
		writeJSONRPCError(c, req.ID, rpcErrMethodNotFound, "method not supported: "+req.Method)
	}
}

func buildToolDescriptors() []mcpToolDescriptor {
	tools := listRegisteredMCPTools()
	out := make([]mcpToolDescriptor, 0, len(tools))
	for _, t := range tools {
		out = append(out, mcpToolDescriptor{
			Name:         t.Name,
			Description:  t.Description,
			InputSchema:  t.InputSchema,
			OutputSchema: t.OutputSchema,
			Annotations:  mcpToolAnnotations(t.Name),
		})
	}
	return out
}

// Advertise read-only hints only for known reads. Unknown/future tools keep
// the conservative write/destructive defaults in ChatGPT and other clients.
func mcpToolAnnotations(name string) map[string]bool {
	switch name {
	case "meta.whoami", "server.list", "server.get", "fs.list", "fs.read":
		return map[string]bool{"readOnlyHint": true, "destructiveHint": false}
	default:
		return map[string]bool{"readOnlyHint": false, "destructiveHint": true}
	}
}

// toolCallParams 是 tools/call 的 params 结构。
type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func handleToolsCall(c *gin.Context, req *jsonRPCRequest, tok *model.APIToken) {
	var p toolCallParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeJSONRPCError(c, req.ID, rpcErrInvalidParams, "invalid arguments: "+err.Error())
			return
		}
	}

	if p.Name == "" {
		writeJSONRPCError(c, req.ID, rpcErrInvalidParams, "tool name required")
		return
	}

	mcpToolsMu.RLock()
	tool, ok := mcpTools[p.Name]
	mcpToolsMu.RUnlock()
	if !ok {
		writeJSONRPCError(c, req.ID, rpcErrMethodNotFound, "unknown tool: "+p.Name)
		return
	}

	uid := uint64(0)
	if u, ok := c.Get(model.CtxKeyAuthorizedUser); ok {
		if user, ok := u.(*model.User); ok && user != nil {
			uid = user.ID
		}
	}
	startedAt := time.Now()
	audit := model.MCPAuditLog{
		UserID:  uid,
		TokenID: tok.ID,
		Tool:    p.Name,
		IP:      c.GetString(model.CtxKeyRealIPStr),
	}

	finish := func(outcome, errCode, errMsg string, result any) {
		if outcome == model.MCPOutcomeOK {
			textPayload, err := marshalMCPToolResult(result)
			if err != nil {
				outcome = model.MCPOutcomeAgentError
				errCode = model.MCPOutcomeAgentError
				errMsg = "failed to encode tool result: " + err.Error()
				result = nil
			} else {
				writeJSONRPCResult(c, req.ID, mcpToolCallResult{
					Content:           []mcpContent{{Type: "text", Text: textPayload}},
					StructuredContent: result,
				})
			}
		}
		audit.Outcome = outcome
		audit.ErrorCode = errCode
		audit.ErrorMsg = truncateString(errMsg, 512)
		audit.DurationMs = time.Since(startedAt).Milliseconds()
		audit.ServerID = extractServerID(p.Arguments)
		mcpAuditWrite(audit, p.Arguments)

		if outcome == model.MCPOutcomeOK {
			return
		}
		// Semantic tool failures may retain a typed structured result (notably
		// server.exec) so clients can distinguish a non-zero command outcome from
		// transport, authorization, or deadline failures.
		writeJSONRPCResult(c, req.ID, mcpToolCallResult{
			Content:           []mcpContent{{Type: "text", Text: errMsg}},
			StructuredContent: result,
			IsError:           true,
		})
	}

	if tool.RequiredScope != "" && !tok.HasScope(tool.RequiredScope) {
		finish(model.MCPOutcomeScopeDenied, model.MCPOutcomeScopeDenied,
			"missing required scope: "+tool.RequiredScope, nil)
		return
	}

	// 让 PAT 吊销能立即中断进行中的 tools/call（如 server.exec 最长 ~305s）：
	// 派生一个可取消 ctx 注入 c.Request，下游 CallAgent 用 c.Request.Context()
	// 即会观察到取消；cancel 注册进吊销表，deleteAPIToken 会立刻触发它。
	if c.Request != nil {
		callCtx, cancel := context.WithCancel(c.Request.Context())
		defer cancel()
		deregister := registerPATConnection(c, cancel)
		defer deregister()
		c.Request = c.Request.WithContext(callCtx)
	}

	result, err := tool.Handler(c, p.Arguments)
	if err != nil {
		code, msg := classifyToolError(err)
		var structuredErr interface{ StructuredResult() any }
		if errors.As(err, &structuredErr) {
			finish(code, code, msg, structuredErr.StructuredResult())
			return
		}
		finish(code, code, msg, nil)
		return
	}
	finish(model.MCPOutcomeOK, "", "", result)
}

// classifyToolError 把任何 handler 返回的 error 归类成审计 outcome + 安全错误消息。
// 优先匹配 mcpError 自带的 Code；否则匹配已知的 rpc.ErrAgent* 类型，最后回退 internal。
func classifyToolError(err error) (code, msg string) {
	if me, ok := err.(*mcpError); ok {
		return me.Code, me.Msg
	}
	if errors.Is(err, rpc.ErrAgentOffline) {
		return model.MCPOutcomeServerOffline, "agent offline"
	}
	if errors.Is(err, rpc.ErrAgentTimeout) {
		return model.MCPOutcomeAgentTimeout, "agent did not respond within timeout"
	}
	if errors.Is(err, rpc.ErrMCPDisabled) {
		// kill switch 触发的中断必须独立成 outcome，避免审计/SIEM 把
		// “管理员关了 MCP”误报成 agent 故障；错误文本透传原始原因。
		return model.MCPOutcomeMCPDisabled, err.Error()
	}
	return model.MCPOutcomeAgentError, err.Error()
}

// extractServerID 从 raw arguments JSON 里提取 server_id（best-effort，只用于审计字段）。
func extractServerID(raw json.RawMessage) uint64 {
	if len(raw) == 0 {
		return 0
	}
	var probe struct {
		ServerID uint64 `json:"server_id"`
	}
	_ = json.Unmarshal(raw, &probe)
	return probe.ServerID
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// --- wire writers ---

func writeJSONRPCResult(c *gin.Context, id json.RawMessage, result any) {
	c.JSON(http.StatusOK, jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func writeJSONRPCError(c *gin.Context, id json.RawMessage, code int, message string) {
	writeJSONRPCErrorWithStatus(c, id, code, message, http.StatusOK)
}

func writeToolCallError(c *gin.Context, id json.RawMessage, errCode, errMsg string) {
	writeJSONRPCResult(c, id, mcpToolCallResult{
		Content: []mcpContent{{Type: "text", Text: errMsg}},
		IsError: true,
		StructuredContent: map[string]string{
			"error_code": errCode,
			"error":      errMsg,
		},
	})
}

func writeJSONRPCErrorWithStatus(c *gin.Context, id json.RawMessage, code int, message string, status int) {
	c.JSON(status, jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: message},
	})
}

// --- 错误语义 ---

// mcpError 是 tool handler 可以返回的语义化错误。
// dispatch 根据 Code 决定 audit outcome 与 JSON-RPC 错误码（如果命中 rpcErr* 域）。
type mcpError struct {
	Code string
	Msg  string
}

func (e *mcpError) Error() string { return e.Msg }

func newMCPError(code, msg string) *mcpError { return &mcpError{Code: code, Msg: msg} }

// 预制错误
var (
	errMCPInvalidArgs = func(s string) *mcpError { return newMCPError(model.MCPOutcomeInvalidArgs, s) }
	errMCPPermDenied  = newMCPError(model.MCPOutcomePermDenied, "permission denied")
	errMCPScopeDenied = func(s string) *mcpError {
		return newMCPError(model.MCPOutcomeScopeDenied, "missing required scope: "+s)
	}
	errMCPServerOffline = newMCPError(model.MCPOutcomeServerOffline, "agent offline")
	errMCPAgentTimeout  = newMCPError(model.MCPOutcomeAgentTimeout, "agent did not respond within timeout")
	errMCPUnsupported   = newMCPError(model.MCPOutcomeUnsupportedAgent, "agent does not support this MCP capability; please upgrade the agent")
)

// --- 共用工具 ---

var errNoToken = errors.New("no api token in context")

// decodeToolArgs 是 tool handler 用来反序列化 arguments 的辅助。
func decodeToolArgs(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// requireServerAccess 是 tool handler 共用的「闸 1 + 闸 2 服务器白名单」组合校验。
// 通过返回 *model.Server；失败返回带语义 Code 的 mcpError，便于 dispatch 归类审计。
func requireServerAccess(c *gin.Context, serverID uint64) (*model.Server, error) {
	if serverID == 0 {
		return nil, errMCPInvalidArgs("server_id required")
	}
	tok := APITokenFromContext(c)
	if tok != nil && !tok.CanAccessServer(serverID) {
		return nil, errMCPPermDenied
	}
	server, _ := singleton.ServerShared.Get(serverID)
	if server == nil {
		return nil, errMCPServerOffline
	}
	if !server.HasPermission(c) {
		return nil, errMCPPermDenied
	}
	return server, nil
}
