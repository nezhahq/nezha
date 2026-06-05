package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hashicorp/go-uuid"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
)

// fs.download_url / fs.upload_url 旁路通道。
//
// 设计目标：给 LLM 客户端一个不经 MCP 上下文的 URL，去用普通 HTTP 客户端
// 上传/下载大文件（单文件 hard cap 100MiB，model.MCPFsTransferMaxSize）。
//
// 传输实现：dashboard ↔ agent 走 gRPC IOStream 双向流（TaskTypeFsTransfer），
// dashboard 一边读 HTTP body 一边推给 agent；不再使用 base64/JSON 包装内容，
// 避免 gRPC 4MiB 单消息上限。
//
// 安全机制：
//   - 一次性 token，存内存 sync.Map，TTL 默认 300s，最多 600s
//   - token 绑定 user_id + token_id + server_id + path + direction
//   - consume 时重算并以常数时间比对 entry 的 HMAC-SHA256，防篡改
//   - 命中后立即从内存删除，禁止重放
//   - revalidateTransferEntry 在 consume 时重新校验 PAT/scope/owner，应对
//     mint→consume 之间的权限变化
//   - 上传可选 ?sha256=<hex> 端到端校验；下载 NZTO 帧附 agent 计算的 sha
type transferDirection string

const (
	transferDirDownload transferDirection = "download"
	transferDirUpload   transferDirection = "upload"

	transferTokenTTLDefault = 300 * time.Second
	transferTokenTTLMax     = 600 * time.Second

	// maxTransferDuration bounds a single upload/download once the agent has
	// attached. 100MiB over a slow link still completes well within this;
	// anything longer is treated as a stalled/abusive transfer and cancelled.
	maxTransferDuration = 10 * time.Minute

	maxTransferPathLen = 4096
)

func validateTransferPath(path string) error {
	if path == "" {
		return errMCPInvalidArgs("path required")
	}
	if len(path) > maxTransferPathLen {
		return errMCPInvalidArgs("path too long")
	}
	return nil
}

type transferEntry struct {
	UserID    uint64
	TokenID   uint64
	ServerID  uint64
	Path      string
	Direction transferDirection
	ExpiresAt time.Time

	// Upload-only optional knobs carried from MCP fs.upload_url tool args
	// to transferUploadHandler so the upload handler can forward them into
	// FsTransferRequest. Empty/false values keep current behaviour for the
	// download direction (these fields are simply ignored).
	UploadMode          string
	UploadCreateDirs    bool
	UploadIfMatchSHA256 string
}

var (
	transferEntries   sync.Map
	transferSecretMu  sync.Mutex
	transferSecretVal string
)

// transferHMACSecret 返回进程内随机生成的 HMAC key。
// 这是有意设计：transferEntries 本身也只活在内存 sync.Map 里，dashboard
// 重启等价于全部 token 失效；让 secret 也随进程随机，可以避免“secret 来自
// 持久化 env 但 entries 已丢”这种半持久化状态，同时保证多副本部署不会
// 意外互认对方签发的 token（每副本一份独立 secret）。
func transferHMACSecret() string {
	transferSecretMu.Lock()
	defer transferSecretMu.Unlock()
	if transferSecretVal != "" {
		return transferSecretVal
	}
	transferSecretVal = utils.MustGenerateRandomString(64)
	return transferSecretVal
}

// transferTokenSig 计算 entry 的 HMAC-SHA256 签名（hex）；mint 与 consume 共用。
func transferTokenSig(e transferEntry) string {
	mac := hmac.New(sha256.New, []byte(transferHMACSecret()))
	fmt.Fprintf(mac, "%s|%d|%d|%d|%s|%d",
		e.Direction, e.UserID, e.TokenID, e.ServerID, e.Path, e.ExpiresAt.UnixNano())
	return hex.EncodeToString(mac.Sum(nil))
}

func mintTransferToken(e transferEntry) (string, error) {
	id, err := utils.GenerateRandomString(24)
	if err != nil {
		return "", err
	}
	tok := id + "." + transferTokenSig(e)
	transferEntries.Store(tok, e)
	return tok, nil
}

func consumeTransferToken(tok string, dir transferDirection) (*transferEntry, error) {
	raw, ok := transferEntries.LoadAndDelete(tok)
	if !ok {
		return nil, errors.New("invalid or already-used transfer token")
	}
	e, _ := raw.(transferEntry)
	// 校验 HMAC：token 形如 id.sig，sig 必须等于 entry 字段在进程 secret 下的
	// HMAC-SHA256。仅靠 sync.Map key 随机性不构成完整性保护——一旦 entry 被
	// 持久化/跨副本共享/从 token 解码，缺这一步即认证绕过。常数时间比较防侧信道。
	idx := strings.LastIndex(tok, ".")
	if idx < 0 {
		return nil, errors.New("malformed transfer token")
	}
	if !hmac.Equal([]byte(tok[idx+1:]), []byte(transferTokenSig(e))) {
		return nil, errors.New("transfer token signature mismatch")
	}
	if e.Direction != dir {
		return nil, errors.New("transfer token direction mismatch")
	}
	if time.Now().After(e.ExpiresAt) {
		return nil, errors.New("transfer token expired")
	}
	return &e, nil
}

// PurgeTransferEntries drops every minted-but-unconsumed transfer URL.
// EnableMCP=false invokes this so an admin pressing the kill switch
// invalidates the 5–10min trailing window of pre-signed download/upload
// URLs that consumeTransferToken would otherwise still honor. Returns the
// number of entries purged for audit.
func PurgeTransferEntries() int {
	purged := 0
	transferEntries.Range(func(key, _ any) bool {
		if _, ok := transferEntries.LoadAndDelete(key); ok {
			purged++
		}
		return true
	})
	return purged
}

// gcExpiredTransferEntries 按 ExpiresAt 删除所有已过期但从未被 consume
// 的 token。kickoffTransferGC 周期调度它，防止 transferEntries 在没人
// 触发 kill switch 的情况下随时间无界增长。
func gcExpiredTransferEntries(now time.Time) int {
	removed := 0
	transferEntries.Range(func(key, raw any) bool {
		e, ok := raw.(transferEntry)
		if !ok {
			transferEntries.Delete(key)
			removed++
			return true
		}
		if now.After(e.ExpiresAt) {
			if _, deleted := transferEntries.LoadAndDelete(key); deleted {
				removed++
			}
		}
		return true
	})
	return removed
}

var transferGCStartOnce sync.Once

// kickoffTransferGC 启动一个进程级 goroutine 定时回收过期 token，
// 避免每个 dashboard 启动都得 PurgeTransferEntries 才能把表清空。
// 时间间隔取 transferTokenTTLDefault / 5，对默认 5min TTL 即 1min；
// 既能在 TTL 内多次扫到过期项，也不会让锁竞争变成热点。
func kickoffTransferGC() {
	transferGCStartOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(transferTokenTTLDefault / 5)
			defer ticker.Stop()
			for range ticker.C {
				gcExpiredTransferEntries(time.Now())
			}
		}()
	})
}

// --- tool: fs.download_url ---

type fsDownloadURLArgs struct {
	ServerID   uint64 `json:"server_id"`
	Path       string `json:"path"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// fsUploadURLArgs is the upload-side superset of fsDownloadURLArgs. agent's
// FsTransferRequest already supports per-upload Mode / CreateDirs /
// IfMatchSHA256, but fs.upload_url historically reused fsDownloadURLArgs and
// silently dropped these fields. Splitting the arg shape lets the MCP tool
// schema advertise them and mintTransferTool plumb them through to
// transferUploadHandler -> openFsTransferStream.
type fsUploadURLArgs struct {
	ServerID      uint64 `json:"server_id"`
	Path          string `json:"path"`
	TTLSeconds    int    `json:"ttl_seconds,omitempty"`
	Mode          string `json:"mode,omitempty"`
	CreateDirs    bool   `json:"create_dirs,omitempty"`
	IfMatchSHA256 string `json:"if_match_sha256,omitempty"`
}

func init() {
	registerMCPTool(&mcpTool{
		Name:        "fs.download_url",
		Description: "Mint a one-time signed URL to stream a file (<=100MiB) via plain HTTP GET. Bypasses MCP context and uses gRPC IOStream end-to-end.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"server_id":   map[string]any{"type": "integer"},
				"path":        map[string]any{"type": "string"},
				"ttl_seconds": map[string]any{"type": "integer", "minimum": 30, "maximum": 600},
			},
			"required": []string{"server_id", "path"},
		},
		OutputSchema:  transferURLOutputSchema(),
		RequiredScope: model.ScopeServerRead,
		Handler:       handleFsDownloadURL,
	})

	registerMCPTool(&mcpTool{
		Name:        "fs.upload_url",
		Description: "Mint a one-time signed URL to stream a file (<=100MiB) via plain HTTP POST. Caller MUST send Content-Length; optional ?sha256=<hex> for end-to-end integrity. mode / create_dirs / if_match_sha256 are forwarded to the agent for atomic chmod / mkdir -p / optimistic concurrency.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"server_id":       map[string]any{"type": "integer"},
				"path":            map[string]any{"type": "string"},
				"ttl_seconds":     map[string]any{"type": "integer", "minimum": 30, "maximum": 600},
				"mode":            map[string]any{"type": "string", "description": "Octal mode like '0644'."},
				"create_dirs":     map[string]any{"type": "boolean"},
				"if_match_sha256": map[string]any{"type": "string", "description": "64 hex chars; precondition checked by the agent before overwrite."},
			},
			"required": []string{"server_id", "path"},
		},
		OutputSchema:  transferURLOutputSchema(),
		RequiredScope: model.ScopeServerWrite,
		Handler:       handleFsUploadURL,
	})
}

func transferURLOutputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":        map[string]any{"type": "string"},
			"method":     map[string]any{"type": "string"},
			"expires_at": map[string]any{"type": "string", "format": "date-time"},
		},
		"required": []string{"url", "method", "expires_at"},
	}
}

func handleFsDownloadURL(c *gin.Context, raw json.RawMessage) (any, error) {
	var args fsDownloadURLArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	return mintTransferTool(c, args.ServerID, args.Path, args.TTLSeconds, transferDirDownload, transferEntry{})
}

func handleFsUploadURL(c *gin.Context, raw json.RawMessage) (any, error) {
	var args fsUploadURLArgs
	if err := decodeToolArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.IfMatchSHA256 != "" {
		if _, decErr := hex.DecodeString(args.IfMatchSHA256); decErr != nil || len(args.IfMatchSHA256) != 64 {
			return nil, errMCPInvalidArgs("if_match_sha256 must be 64 hex chars")
		}
	}
	return mintTransferTool(c, args.ServerID, args.Path, args.TTLSeconds, transferDirUpload, transferEntry{
		UploadMode:          args.Mode,
		UploadCreateDirs:    args.CreateDirs,
		UploadIfMatchSHA256: args.IfMatchSHA256,
	})
}

func mintTransferTool(c *gin.Context, serverID uint64, path string, ttlSeconds int, dir transferDirection, uploadExtras transferEntry) (any, error) {
	srv, err := requireServerAccess(c, serverID)
	if err != nil {
		return nil, err
	}
	if err := requireAgentSupportsMCP(srv); err != nil {
		return nil, err
	}
	if err := validateTransferPath(path); err != nil {
		return nil, err
	}
	ttl := time.Duration(ttlSeconds) * time.Second
	if ttl <= 0 {
		ttl = transferTokenTTLDefault
	}
	if ttl > transferTokenTTLMax {
		ttl = transferTokenTTLMax
	}

	tok := APITokenFromContext(c)
	if tok == nil {
		return nil, errNoToken
	}
	uid := uint64(0)
	if u, ok := c.Get(model.CtxKeyAuthorizedUser); ok {
		if user, ok := u.(*model.User); ok && user != nil {
			uid = user.ID
		}
	}
	entry := transferEntry{
		UserID:              uid,
		TokenID:             tok.ID,
		ServerID:            serverID,
		Path:                path,
		Direction:           dir,
		ExpiresAt:           time.Now().Add(ttl),
		UploadMode:          uploadExtras.UploadMode,
		UploadCreateDirs:    uploadExtras.UploadCreateDirs,
		UploadIfMatchSHA256: uploadExtras.UploadIfMatchSHA256,
	}
	t, err := mintTransferToken(entry)
	if err != nil {
		return nil, err
	}
	scheme := "https"
	if c.Request.TLS == nil && c.Request.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	host := c.Request.Host
	url := fmt.Sprintf("%s://%s/mcp/%s/%s", scheme, host, dir, t)
	return map[string]any{
		"url":        url,
		"method":     map[transferDirection]string{transferDirDownload: "GET", transferDirUpload: "POST"}[dir],
		"expires_at": entry.ExpiresAt,
	}, nil
}

// --- HTTP handlers ---

// transferDownloadHandler 处理 GET /mcp/download/:token。
// 走 IOStream 双向流：dashboard 把 agent 推过来的 chunk 转发给 HTTP 客户端，
// 单文件 hard cap 100MiB（model.MCPFsTransferMaxSize）。
// transferRevokableContext 把进行中的传输纳入 PAT 撤销注册表。返回的 ctx 在
// 该 PAT 被 deleteAPIToken 撤销时取消，从而切断已开始的 upload/download；
// 否则只在传输自然结束时由 stop() 注销。stop() 必须 defer 调用。
func transferRevokableContext(c *gin.Context, e *transferEntry) (context.Context, func()) {
	// Cap the whole transfer with a hard deadline. After the agent attaches,
	// the relay blocks in IOStreamWrapper.Read, which only honours this ctx
	// (openFsTransferStream closes the stream on ctx.Done). Without the
	// deadline a stalled or malicious agent that attaches but never sends a
	// complete header/chunk/final frame pins this goroutine, the IOStream and
	// the spool tmpfile until the client disconnects, allowing concurrent
	// hung transfers to exhaust resources within the rate limit.
	ctx, cancel := context.WithTimeout(c.Request.Context(), maxTransferDuration)
	dereg := patConnectionRegistryShared.register(e.TokenID, cancel)
	return ctx, func() {
		dereg()
		cancel()
	}
}

func transferDownloadHandler(c *gin.Context) {
	tok := c.Param("token")
	entry, err := consumeTransferToken(tok, transferDirDownload)
	if err != nil {
		writeTransferFailureAudit(c, nil, "fs.download", classifyTransferConsumeError(err), err)
		c.String(http.StatusUnauthorized, err.Error())
		return
	}
	if err := revalidateTransferEntry(entry); err != nil {
		writeTransferFailureAudit(c, entry, "fs.download", classifyTransferRevalidateError(err), err)
		c.String(http.StatusUnauthorized, err.Error())
		return
	}

	ctx, stop := transferRevokableContext(c, entry)
	defer stop()

	stream, cleanup, err := openFsTransferStream(ctx, entry.ServerID, &model.FsTransferRequest{
		Op:   model.MCPFsTransferOpDownload,
		Path: entry.Path,
	})
	if err != nil {
		writeTransferFailureAudit(c, entry, "fs.download", classifyTransferOpenStreamError(err), err)
		c.String(http.StatusBadGateway, err.Error())
		return
	}
	defer cleanup()

	hdr, err := readXferFixedHeader(stream)
	if err != nil {
		writeTransferFailureAudit(c, entry, "fs.download", model.MCPOutcomeAgentTimeout, err)
		c.String(http.StatusBadGateway, "agent did not return download header: "+err.Error())
		return
	}
	if hdr.IsErr() {
		writeTransferFailureAudit(c, entry, "fs.download", model.MCPOutcomeAgentError, errors.New(hdr.ErrMsg))
		c.String(http.StatusBadGateway, hdr.ErrMsg)
		return
	}
	if !bytes.Equal(hdr.Magic, model.MCPFsXferMagicDownloadHdr) {
		writeTransferFailureAudit(c, entry, "fs.download", model.MCPOutcomeAgentError, errors.New("unexpected header magic"))
		c.String(http.StatusBadGateway, "agent returned unexpected header magic")
		return
	}
	if hdr.Size > model.MCPFsTransferMaxSize {
		writeTransferFailureAudit(c, entry, "fs.download", model.MCPOutcomeAgentError, errors.New("file exceeds MCP transfer cap"))
		c.String(http.StatusBadGateway, "file exceeds MCP transfer cap (100MiB)")
		return
	}

	if err := relayDownloadFrames(c, stream, hdr.Size); err != nil {
		writeTransferFailureAudit(c, entry, "fs.download", model.MCPOutcomeAgentError, err)
		return
	}

	_ = singleton.DB.Create(&model.MCPAuditLog{
		UserID:   entry.UserID,
		TokenID:  entry.TokenID,
		Tool:     "fs.download",
		ServerID: entry.ServerID,
		Outcome:  model.MCPOutcomeOK,
		IP:       c.GetString(model.CtxKeyRealIPStr),
	}).Error
}

// transferUploadHandler 处理 POST /mcp/upload/:token；body 转发到 agent，
// 单文件 hard cap 100MiB。
func transferUploadHandler(c *gin.Context) {
	tok := c.Param("token")
	entry, err := consumeTransferToken(tok, transferDirUpload)
	if err != nil {
		writeTransferFailureAudit(c, nil, "fs.upload", classifyTransferConsumeError(err), err)
		c.String(http.StatusUnauthorized, err.Error())
		return
	}
	if err := revalidateTransferEntry(entry); err != nil {
		writeTransferFailureAudit(c, entry, "fs.upload", classifyTransferRevalidateError(err), err)
		c.String(http.StatusUnauthorized, err.Error())
		return
	}

	// 1) 体积闸门：Content-Length 必须存在并且不超过 cap。流式上传时这是
	// 唯一能在打开 IOStream 之前就拒掉超大请求的依据，避免 agent 端
	// 拒绝时已经占了一个连接。
	if c.Request.ContentLength < 0 {
		writeTransferFailureAudit(c, entry, "fs.upload", model.MCPOutcomeInvalidArgs, errors.New("Content-Length required"))
		c.String(http.StatusLengthRequired, "Content-Length required")
		return
	}
	if c.Request.ContentLength > model.MCPFsTransferMaxSize {
		writeTransferFailureAudit(c, entry, "fs.upload", model.MCPOutcomeInvalidArgs, errors.New("body exceeds MCP transfer cap"))
		c.String(http.StatusRequestEntityTooLarge, "body exceeds MCP transfer cap (100MiB)")
		return
	}
	size := c.Request.ContentLength

	// 可选的端到端 sha256：通过 query 参数 sha256=<hex> 传入，agent 收到全部
	// 字节后会比对；不传则只回带 sha 但不强校验。
	expected := strings.ToLower(strings.TrimSpace(c.Query("sha256")))
	if expected != "" {
		if _, decErr := hex.DecodeString(expected); decErr != nil || len(expected) != 64 {
			writeTransferFailureAudit(c, entry, "fs.upload", model.MCPOutcomeInvalidArgs, errors.New("sha256 must be 64 hex chars"))
			c.String(http.StatusBadRequest, "sha256 must be 64 hex chars")
			return
		}
	}

	// 上限再加一个字节做 MaxBytesReader 屏障：若客户端撒谎、实际 body 超过
	// Content-Length，HTTP 层会立即截断并报 413。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, model.MCPFsTransferMaxSize+1)

	ctx, stop := transferRevokableContext(c, entry)
	defer stop()

	stream, cleanup, err := openFsTransferStream(ctx, entry.ServerID, &model.FsTransferRequest{
		Op:             model.MCPFsTransferOpUpload,
		Path:           entry.Path,
		Size:           size,
		ExpectedSHA256: expected,
		Mode:           entry.UploadMode,
		CreateDirs:     entry.UploadCreateDirs,
		IfMatchSHA256:  entry.UploadIfMatchSHA256,
	})
	if err != nil {
		writeTransferFailureAudit(c, entry, "fs.upload", classifyTransferOpenStreamError(err), err)
		c.String(http.StatusBadGateway, err.Error())
		return
	}
	defer cleanup()

	hdr, err := readXferFixedHeader(stream)
	if err != nil {
		writeTransferFailureAudit(c, entry, "fs.upload", model.MCPOutcomeAgentTimeout, err)
		c.String(http.StatusBadGateway, "agent did not return upload ready frame: "+err.Error())
		return
	}
	if hdr.IsErr() {
		writeTransferFailureAudit(c, entry, "fs.upload", model.MCPOutcomeAgentError, errors.New(hdr.ErrMsg))
		c.String(http.StatusBadGateway, hdr.ErrMsg)
		return
	}
	if !bytes.Equal(hdr.Magic, model.MCPFsXferMagicUploadHdr) {
		writeTransferFailureAudit(c, entry, "fs.upload", model.MCPOutcomeAgentError, errors.New("unexpected header magic"))
		c.String(http.StatusBadGateway, "agent returned unexpected header magic")
		return
	}
	if hdr.Size != size {
		writeTransferFailureAudit(c, entry, "fs.upload", model.MCPOutcomeAgentError, errors.New("agent acknowledged unexpected size"))
		c.String(http.StatusBadGateway, "agent acknowledged unexpected size")
		return
	}

	if _, copyErr := io.CopyN(stream, c.Request.Body, size); copyErr != nil {
		writeTransferFailureAudit(c, entry, "fs.upload", model.MCPOutcomeAgentError, copyErr)
		c.String(http.StatusBadGateway, "stream relay failed: "+copyErr.Error())
		return
	}

	final, err := readXferFixedHeader(stream)
	if err != nil {
		writeTransferFailureAudit(c, entry, "fs.upload", model.MCPOutcomeAgentTimeout, err)
		c.String(http.StatusBadGateway, "agent did not acknowledge upload: "+err.Error())
		return
	}
	if final.IsErr() {
		writeTransferFailureAudit(c, entry, "fs.upload", model.MCPOutcomeAgentError, errors.New(final.ErrMsg))
		c.String(http.StatusBadGateway, final.ErrMsg)
		return
	}
	if !bytes.Equal(final.Magic, model.MCPFsXferMagicOK) {
		writeTransferFailureAudit(c, entry, "fs.upload", model.MCPOutcomeAgentError, errors.New("unexpected final magic"))
		c.String(http.StatusBadGateway, "agent returned unexpected final magic")
		return
	}

	c.JSON(http.StatusOK, model.FsWriteResult{Size: int64(final.Size), SHA256: hex.EncodeToString(final.SHA256)})
	_ = singleton.DB.Create(&model.MCPAuditLog{
		UserID:   entry.UserID,
		TokenID:  entry.TokenID,
		Tool:     "fs.upload",
		ServerID: entry.ServerID,
		Outcome:  model.MCPOutcomeOK,
		IP:       c.GetString(model.CtxKeyRealIPStr),
	}).Error
}

// writeTransferFailureAudit 是 fs.upload / fs.download HTTP handler 失败路径
// 共用的审计写入。outcome 必须用 model.MCPOutcome* 常量；entry 可以是 nil
// （token consume 阶段就失败时拿不到 entry，UserID/TokenID/ServerID 写 0）。
//
// Anonymous failures (entry == nil) go through transferAnonAuditThrottleShared
// per-IP sampler so an unauthenticated attacker cannot flood mcp_audit_log
// by POSTing /mcp/upload/<random>. Authenticated failures bypass the
// throttle so SIEM signal stays intact.
func writeTransferFailureAudit(c *gin.Context, entry *transferEntry, tool, outcome string, err error) {
	ip := c.GetString(model.CtxKeyRealIPStr)
	if entry == nil && !transferAnonAuditThrottleShared.shouldRecord(ip) {
		return
	}
	entryLog := model.MCPAuditLog{
		Tool:    tool,
		Outcome: outcome,
		IP:      ip,
	}
	if entry != nil {
		entryLog.UserID = entry.UserID
		entryLog.TokenID = entry.TokenID
		entryLog.ServerID = entry.ServerID
	}
	if err != nil {
		msg := err.Error()
		if len(msg) > 512 {
			msg = msg[:512]
		}
		entryLog.ErrorMsg = msg
		entryLog.ErrorCode = outcome
	}
	mcpAuditWrite(entryLog, nil)
}

// classifyTransferConsumeError 把 consumeTransferToken 的错误映射成 outcome。
// 让 SIEM 能区分“伪造/过期 token”与“direction 不匹配”等场景。
func classifyTransferConsumeError(err error) string {
	if err == nil {
		return model.MCPOutcomeInternalError
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "expired"):
		return model.MCPOutcomeScopeDenied
	case strings.Contains(msg, "direction mismatch"):
		return model.MCPOutcomeInvalidArgs
	default:
		return model.MCPOutcomePermDenied
	}
}

// classifyTransferRevalidateError 把 revalidateTransferEntry 的错误映射成
// outcome。最重要的一项是“MCP is disabled” → MCPOutcomeMCPDisabled，让
// 运营在 audit 表里直接看出 kill switch 命中情况，而不是只看到 perm_denied。
func classifyTransferRevalidateError(err error) string {
	if err == nil {
		return model.MCPOutcomeInternalError
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "MCP is disabled"):
		return model.MCPOutcomeMCPDisabled
	case strings.Contains(msg, "no longer has required scope"):
		return model.MCPOutcomeScopeDenied
	case strings.Contains(msg, "no longer covers"):
		return model.MCPOutcomeScopeDenied
	case strings.Contains(msg, "expired"):
		return model.MCPOutcomeScopeDenied
	default:
		return model.MCPOutcomePermDenied
	}
}

// classifyTransferOpenStreamError 把 openFsTransferStream 的失败映射成
// outcome：offline / 30s attach 超时分别对应 ServerOffline / AgentTimeout。
func classifyTransferOpenStreamError(err error) string {
	if err == nil {
		return model.MCPOutcomeInternalError
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "server offline"):
		return model.MCPOutcomeServerOffline
	case strings.Contains(msg, "did not attach"):
		return model.MCPOutcomeAgentTimeout
	default:
		return model.MCPOutcomeAgentError
	}
}

// frameReceiver is the frame-preserving subset of grpcx.IOStreamWrapper that
// the download relay needs. We accept the interface (not the concrete type)
// so test simulators can plug in a net.Pipe-backed stream without depending
// on the gRPC stack.
type frameReceiver interface {
	RecvFrame() ([]byte, error)
}

// relayDownloadFrames forwards declared-size payload from agent to HTTP
// client. The agent wraps every data chunk in an NZTC frame (4-byte magic +
// 8-byte big-endian length + payload) so payload that happens to begin with
// the same bytes as a control frame (NZTE / NZTO) cannot be misclassified.
// Control frames (NZTE error, NZTO success) sit on the same IOStream and
// are recognized by their magic; legitimate payload always arrives inside
// NZTC frames and is never matched against the control-frame magics.
//
// Payload is spooled to a per-request tmpfile rather than kept in a 100MiB
// memory buffer: a midstream NZTE must be able to switch the HTTP response
// to 502, which forces us to defer the body write until the final NZTO
// frame is observed; but we MUST NOT pay 100MiB of heap per concurrent
// download to do so.
func relayDownloadFrames(c *gin.Context, stream io.ReadWriteCloser, size int64) error {
	spool, err := newTransferSpool()
	if err != nil {
		c.String(http.StatusInternalServerError, "transfer spool: "+err.Error())
		return err
	}
	defer spool.Close()

	// Hash the relayed bytes inline; we compare against the trailing
	// NZTO declared sha256 in validateDownloadFinal so corrupt or
	// truncated agent payloads can't reach the client.
	hasher := sha256.New()
	streamed := int64(0)

	remaining := size
	header := make([]byte, 4+8)
	for remaining > 0 {
		if _, err := io.ReadFull(stream, header); err != nil {
			c.String(http.StatusBadGateway, "stream relay failed: "+err.Error())
			return err
		}
		if bytes.HasPrefix(header, model.MCPFsXferMagicErr) {
			msg := readMidstreamErrMsg(stream, header)
			c.String(http.StatusBadGateway, msg)
			return errMCPMidstreamAbort
		}
		if !bytes.HasPrefix(header, model.MCPFsXferMagicChunk) {
			c.String(http.StatusBadGateway, "stream relay failed: expected NZTC chunk frame")
			return errMCPMidstreamAbort
		}
		chunkLen := binary.BigEndian.Uint64(header[4:12])
		if chunkLen == 0 {
			// A zero-length data frame makes no progress toward `remaining`.
			// Treating it as a no-op `continue` lets a malicious or buggy
			// agent stream an unbounded run of zero-length NZTC frames,
			// pinning this goroutine, the gRPC stream and the spool tmpfile
			// forever (the final NZTO is never reached). Reject it: a real
			// transfer that still owes bytes never needs an empty data frame.
			c.String(http.StatusBadGateway, "stream relay failed: zero-length data frame while payload incomplete")
			return errMCPMidstreamAbort
		}
		if int64(chunkLen) > remaining {
			c.String(http.StatusBadGateway, "agent oversent: more data bytes than declared size")
			return errMCPMidstreamAbort
		}
		n, err := io.CopyN(io.MultiWriter(spool, hasher), stream, int64(chunkLen))
		if err != nil {
			c.String(http.StatusBadGateway, "stream relay failed: "+err.Error())
			return err
		}
		streamed += n
		remaining -= n
	}

	final := make([]byte, 4+8+32)
	if _, err := io.ReadFull(stream, final); err != nil {
		c.String(http.StatusBadGateway, "agent did not send final transfer frame: "+err.Error())
		return errMCPMidstreamAbort
	}
	if bytes.HasPrefix(final, model.MCPFsXferMagicErr) {
		msg := readMidstreamErrMsg(stream, final[:4+8])
		c.String(http.StatusBadGateway, msg)
		return errMCPMidstreamAbort
	}
	if err := validateDownloadFinal(final, streamed, hasher.Sum(nil)); err != nil {
		c.String(http.StatusBadGateway, err.Error())
		return errMCPMidstreamAbort
	}

	if err := spool.Rewind(); err != nil {
		c.String(http.StatusInternalServerError, "transfer spool rewind: "+err.Error())
		return err
	}
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Length", strconv.FormatInt(size, 10))
	if _, writeErr := io.Copy(c.Writer, spool); writeErr != nil {
		return writeErr
	}
	return nil
}

// validateDownloadFinal cross-checks the trailing NZTO frame against the
// payload the dashboard actually relayed:
//   - magic must be NZTO (defence-in-depth; the relay already checked).
//   - frame must be the full 44 bytes (magic 4 + size 8 + sha256 32).
//   - declared size must match streamed byte count exactly.
//   - declared sha256 must match the streamed sha256, with one allowed
//     "explicit skip" form: all-zero declared hash means agent could not
//     compute a hash and we accept the size-only check.
//
// Without this gate a truncated or wrong-hash NZTO is silently accepted
// and the dashboard serves possibly-corrupt bytes to the HTTP client.
func validateDownloadFinal(final []byte, streamedSize int64, streamedSHA256 []byte) error {
	if len(final) < 4 || !bytes.Equal(final[:4], model.MCPFsXferMagicOK) {
		return errors.New("download final frame: unexpected magic")
	}
	if len(final) < 4+8+32 {
		return errors.New("download final frame: truncated header (need size + sha256)")
	}
	declaredSize := binary.BigEndian.Uint64(final[4:12])
	if uint64(streamedSize) != declaredSize {
		return errors.New("download final frame: declared size does not match streamed bytes")
	}
	declaredSHA := final[12:44]
	allZero := true
	for _, b := range declaredSHA {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return nil
	}
	if len(streamedSHA256) < 32 {
		return errors.New("download final frame: streamed sha256 too short to compare")
	}
	if !bytes.Equal(declaredSHA, streamedSHA256[:32]) {
		return errors.New("download final frame: declared sha256 does not match streamed bytes")
	}
	return nil
}

// midstreamErrMsgCap 限制错误帧 payload 的累计读取量。错误消息只作 string
// 用，没有上限的话恶意/有 bug 的 agent 可在 NZTE 后持续发 256 字节块（且不
// 关流），让 dashboard goroutine 内存无界增长或永久阻塞。读满 cap 即停止。
const midstreamErrMsgCap = 8 << 10

func readMidstreamErrMsg(stream io.Reader, header []byte) string {
	rest := make([]byte, 0, 256)
	tail := make([]byte, 256)
	for len(rest) < midstreamErrMsgCap {
		n, err := stream.Read(tail)
		if n > 0 {
			room := midstreamErrMsgCap - len(rest)
			if n > room {
				n = room
			}
			rest = append(rest, tail[:n]...)
		}
		if err != nil || n < len(tail) {
			break
		}
	}
	return string(header[len(model.MCPFsXferMagicErr):]) + string(rest)
}

var errMCPMidstreamAbort = errors.New("mcp transfer: aborted mid-stream by agent")

// fsTransferXferHeader 是 dashboard 解析 NZTU/NZTD/NZTO/NZTE 后得到的统一
// 结构。Magic 与 model.MCPFsXferMagic* 对照判断帧类型。
type fsTransferXferHeader struct {
	Magic  []byte
	Size   int64
	SHA256 []byte
	ErrMsg string
}

func (h *fsTransferXferHeader) IsErr() bool {
	return bytes.Equal(h.Magic, model.MCPFsXferMagicErr)
}

// readXferFixedHeader 读取一帧 IOStream 数据并解析。每条 agent 控制帧都在
// 单条 IOStreamData 内完整发送（agent 端用 stream.Send(buf) 整块写），所以
// 一次 8KiB 缓冲即可拿到完整帧；不需要跨帧拼接。
//
// 心跳帧（空 Data）由 agent 那侧的 ioStreamKeepAlive 周期性下发，io.Read
// 不会暴露空读，因此这里不必特殊跳过。
func readXferFixedHeader(stream io.Reader) (*fsTransferXferHeader, error) {
	buf := make([]byte, 4+8+32+512)
	n, err := stream.Read(buf)
	if err != nil {
		return nil, err
	}
	return readXferFixedHeaderFromBytes(buf[:n])
}

// readXferFixedHeaderFromBytes parses a fully-received transfer control
// frame. Extracted so a malicious-input regression suite can pin the
// uint64→int64 overflow gate: raw u64 size > MCPFsTransferMaxSize or
// > MaxInt64 must be rejected BEFORE narrowing, otherwise the cap check
// later in the handler sees a wrapped negative value and lets the
// transfer through.
func readXferFixedHeaderFromBytes(raw []byte) (*fsTransferXferHeader, error) {
	if len(raw) < 4 {
		return nil, errors.New("frame too short")
	}
	magic := raw[:4]
	out := &fsTransferXferHeader{Magic: append([]byte(nil), magic...)}
	switch {
	case bytes.Equal(magic, model.MCPFsXferMagicErr):
		out.ErrMsg = string(raw[4:])
		return out, nil
	case bytes.Equal(magic, model.MCPFsXferMagicUploadHdr):
		if len(raw) < 4+8 {
			return nil, errors.New("upload header too short")
		}
		size, err := xferSizeFromU64(binary.BigEndian.Uint64(raw[4:12]))
		if err != nil {
			return nil, err
		}
		out.Size = size
		return out, nil
	case bytes.Equal(magic, model.MCPFsXferMagicDownloadHdr):
		if len(raw) < 4+8+32 {
			return nil, errors.New("download header too short")
		}
		size, err := xferSizeFromU64(binary.BigEndian.Uint64(raw[4:12]))
		if err != nil {
			return nil, err
		}
		out.Size = size
		out.SHA256 = append([]byte(nil), raw[12:44]...)
		return out, nil
	case bytes.Equal(magic, model.MCPFsXferMagicOK):
		if len(raw) < 4+8+32 {
			return nil, errors.New("ok header too short")
		}
		size, err := xferSizeFromU64(binary.BigEndian.Uint64(raw[4:12]))
		if err != nil {
			return nil, err
		}
		out.Size = size
		out.SHA256 = append([]byte(nil), raw[12:44]...)
		return out, nil
	default:
		return nil, errors.New("unexpected frame magic")
	}
}

// xferSizeFromU64 caps the raw u64 size carried by an NZTU/NZTD/NZTO frame
// at MCPFsTransferMaxSize AND math.MaxInt64. Both bounds matter: the cap
// keeps the protocol invariant, the MaxInt64 floor keeps later int64
// arithmetic safe even if MCPFsTransferMaxSize is ever raised above
// MaxInt64 by accident.
func xferSizeFromU64(raw uint64) (int64, error) {
	if raw > uint64(model.MCPFsTransferMaxSize) {
		return 0, errors.New("declared size exceeds MCP transfer cap")
	}
	if raw > math.MaxInt64 {
		return 0, errors.New("declared size overflows int64")
	}
	return int64(raw), nil
}

// openFsTransferStream 走 IOStream 通道与目标 agent 建立一条专用大文件流。
// 返回的 io.ReadWriteCloser 既能 Read（接收 agent→dashboard 字节）又能 Write
// （发送 dashboard→agent 字节）；调用方通过 readXferFixedHeader 解析控制帧。
//
// 内部步骤：
//  1. 分配 streamId，CreateStream(streamId, 0, serverID) 在 NezhaHandler 注册
//     一个 ioStreamContext，targetServerID 用于 agent 侧 stream 归属校验。
//  2. 通过 server 当前的 RequestTask 流发 TaskTypeFsTransfer，把 streamId +
//     req JSON 下发给 agent。
//  3. 等 agent 通过 IOStream() RPC 完成 magic 引导帧并 AgentConnected。
//  4. 返回 agent 端流和 cleanup（CloseStream）。
//
// 任何步骤失败都会 CloseStream 释放资源；调用方只需在 defer cleanup() 即可。
func openFsTransferStream(ctx context.Context, serverID uint64, req *model.FsTransferRequest) (io.ReadWriteCloser, func(), error) {
	if singleton.Conf == nil || !singleton.Conf.MCPEnabled() {
		return nil, func() {}, errors.New("MCP is disabled by the dashboard administrator")
	}
	server, _ := singleton.ServerShared.Get(serverID)
	if server == nil {
		return nil, func() {}, errors.New("server offline")
	}
	if server.GetTaskStream() == nil {
		return nil, func() {}, errors.New("server offline")
	}

	streamId, err := uuid.GenerateUUID()
	if err != nil {
		return nil, func() {}, err
	}
	req.StreamID = streamId

	if err := rpc.NezhaHandlerSingleton.CreateStreamWithPurpose(streamId, 0, serverID, rpc.PurposeMCPTransfer); err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { _ = rpc.NezhaHandlerSingleton.CloseStream(streamId) }

	body, err := json.Marshal(req)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	// 关闭 entry-check 与 SendTask 之间的 TOCTOU：stream 已注册后再复查一次
	// kill switch / ctx 取消，确保 disable sweep 要么扫到这条已注册 stream、
	// 要么这里读到 disabled，绝不会在禁用/吊销后仍把 transfer 任务发给 agent。
	if singleton.Conf == nil || !singleton.Conf.MCPEnabled() {
		cleanup()
		return nil, func() {}, errors.New("MCP is disabled by the dashboard administrator")
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if err := server.SendTask(&pb.Task{
		Type: model.TaskTypeFsTransfer,
		Data: string(body),
	}); err != nil {
		cleanup()
		if errors.Is(err, model.ErrTaskStreamOffline) {
			return nil, func() {}, errors.New("server offline")
		}
		return nil, func() {}, err
	}

	agentStream, ok := rpc.NezhaHandlerSingleton.WaitForAgent(ctx, streamId, 30*time.Second)
	if !ok {
		cleanup()
		return nil, func() {}, errors.New("agent did not attach within 30s")
	}

	// After attach, the relay blocks in IOStreamWrapper.Read, which only
	// honours the gRPC stream context — not this per-transfer ctx. Without
	// the watcher below, a PAT revocation (deleteAPIToken cancels ctx) or a
	// client disconnect would leave a stalled/compromised agent pinning this
	// goroutine + IOStream until process restart. Closing the stream on
	// ctx.Done() unblocks the agent-side handler (iw.Wait) so Read returns.
	watcherDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = rpc.NezhaHandlerSingleton.CloseStream(streamId)
		case <-watcherDone:
		}
	}()
	wrappedCleanup := func() {
		close(watcherDone)
		cleanup()
	}
	return agentStream, wrappedCleanup, nil
}

// revalidateTransferEntry 在消费一次性 URL 时重新检查 mint 阶段的全部前置。
// 这是 mint→consume 之间发生权限变化（PAT 吊销、scope/whitelist 收紧、
// server 转手）时的兜底闸门：HMAC 签发与 sync.Map 一次性消费机制本身只能
// 防伪造与防重放，无法感知后端状态。
func revalidateTransferEntry(e *transferEntry) error {
	if singleton.Conf == nil || !singleton.Conf.MCPEnabled() {
		return errors.New("MCP is disabled by the dashboard administrator")
	}
	var tok model.APIToken
	if err := singleton.DB.First(&tok, e.TokenID).Error; err != nil {
		return errors.New("originating api token no longer exists")
	}
	// Bind the reloaded token back to the minting user. If the original PAT
	// was deleted and its numeric primary key reused by a different user's
	// token, the row would still load here; without this check the stale
	// one-time URL would be revalidated against an unrelated token.
	if tok.UserID != e.UserID {
		return errors.New("originating api token no longer exists")
	}
	if tok.IsExpired(time.Now()) {
		return errors.New("originating api token expired")
	}
	wantScope := model.ScopeServerRead
	if e.Direction == transferDirUpload {
		wantScope = model.ScopeServerWrite
	}
	if !tok.HasScope(wantScope) {
		return errors.New("originating api token no longer has required scope")
	}
	if !tok.CanAccessServer(e.ServerID) {
		return errors.New("originating api token no longer covers target server")
	}
	srv, _ := singleton.ServerShared.Get(e.ServerID)
	if srv == nil {
		return errors.New("target server no longer exists")
	}
	var user model.User
	if err := singleton.DB.First(&user, e.UserID).Error; err != nil {
		return errors.New("originating user no longer exists")
	}
	if user.Role != model.RoleAdmin && srv.GetUserID() != e.UserID {
		return errors.New("target server is no longer owned by the originating user")
	}
	return nil
}
