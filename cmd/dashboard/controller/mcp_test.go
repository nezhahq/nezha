package controller

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	"github.com/nezhahq/nezha/service/singleton"
)

func setupMCPTest(t *testing.T) (func(), uint64) {
	return setupMCPTestWithDatabase(t, ":memory:")
}

func setupMCPTestWithDatabase(t *testing.T, databasePath string) (func(), uint64) {
	t.Helper()
	originalDB := singleton.DB
	originalServer := singleton.ServerShared
	originalConf := singleton.Conf
	originalAuditSync := mcpAuditSync
	originalLimiter := mcpRateLimiterShared
	originalLocalizer := singleton.Localizer
	originalPATRegistry := patConnectionRegistryShared
	singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	mcpAuditSync = true
	mcpRateLimiterShared = newMCPRateLimiter(1000, 10000)
	// Fresh per test: the DB resets token IDs to 1 each run, so a stale
	// revoke tombstone from a prior test would otherwise cancel a reused id.
	patConnectionRegistryShared = newPATConnectionRegistry()
	db, err := gorm.Open(sqlite.Open(databasePath), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.APIToken{}, &model.MCPAuditLog{}, &model.Server{}, &model.WAF{}))
	singleton.DB = db
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{
		JWTTimeout: 1,
		ConfigDashboard: model.ConfigDashboard{
			DashboardHost: "example.com",
		},
	}}
	singleton.Conf.SetMCPEnabled(true)

	user := model.User{Common: model.Common{ID: 100}, Username: "alice", Role: model.RoleMember}
	require.NoError(t, db.Create(&user).Error)

	sc := singleton.NewEmptyServerClassForTest()
	srv := &model.Server{}
	srv.ID = 7
	srv.Name = "alpha"
	srv.SetUserID(100)
	sc.InsertForTest(srv)
	singleton.ServerShared = sc

	cleanup := func() {
		_ = sqlDB.Close()
		singleton.DB = originalDB
		singleton.ServerShared = originalServer
		singleton.Conf = originalConf
		singleton.Localizer = originalLocalizer
		mcpAuditSync = originalAuditSync
		mcpRateLimiterShared = originalLimiter
		patConnectionRegistryShared = originalPATRegistry
	}
	return cleanup, user.ID
}

func mkToken(t *testing.T, uid uint64, scopes []string, serverIDs []uint64) (*model.APIToken, string) {
	t.Helper()
	plain := "nzp_" + strings.Repeat("a", 32) + "_" + ctoa(uid)
	tok := model.APIToken{UserID: uid, Name: "t", TokenHash: model.HashAPIToken(plain)}
	tok.SetScopes(scopes)
	if len(serverIDs) > 0 {
		tok.SetServerIDs(serverIDs)
	}
	require.NoError(t, singleton.DB.Create(&tok).Error)
	return &tok, plain
}

func mcpCallCtx(t *testing.T, tok *model.APIToken, uid uint64, body any) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	b, _ := json.Marshal(body)
	c.Request = httptest.NewRequest("POST", "/mcp", bytes.NewReader(b))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: uid}, Role: model.RoleMember})
	c.Set(apiTokenCtxKey, tok)
	c.Set(model.CtxKeyAPIToken, tok)
	return c, w
}

func decodeRPC(w *httptest.ResponseRecorder) (jsonRPCResponse, *mcpToolCallResult) {
	var env jsonRPCResponse
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Result == nil {
		return env, nil
	}
	rb, _ := json.Marshal(env.Result)
	var tcr mcpToolCallResult
	_ = json.Unmarshal(rb, &tcr)
	return env, &tcr
}

func TestMCP_RejectsMissingToken(t *testing.T) {
	cleanup, _ := setupMCPTest(t)
	defer cleanup()

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "initialize"})
	c.Request = httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	mcpEndpoint(c)
	var env jsonRPCResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.NotNil(t, env.Error)
	require.Equal(t, rpcErrUnauthorized, env.Error.Code)
}

func TestMCP_Initialize_ReturnsServerInfo(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "initialize"})
	mcpEndpoint(c)
	env, _ := decodeRPC(w)
	require.Nil(t, env.Error)
	rb, _ := json.Marshal(env.Result)
	require.Contains(t, string(rb), "nezha-mcp")
	require.Contains(t, string(rb), "protocolVersion")
}

func TestMCP_ToolsList_IncludesRegisteredTools(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/list"})
	mcpEndpoint(c)
	env, _ := decodeRPC(w)
	require.Nil(t, env.Error)
	rb, _ := json.Marshal(env.Result)
	for _, name := range []string{"meta.whoami", "server.list", "server.exec", "fs.list", "fs.read", "fs.write", "fs.delete", "fs.download_url", "fs.upload_url"} {
		require.Contains(t, string(rb), name)
	}
}

func TestMCP_Whoami_HappyPath(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead, model.ScopeServerRead}, []uint64{7, 8})
	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{Name: "meta.whoami", Arguments: json.RawMessage("{}")}),
	})
	mcpEndpoint(c)
	_, tcr := decodeRPC(w)
	require.NotNil(t, tcr)
	require.False(t, tcr.IsError, "got error content: %v", tcr.Content)
	scb, _ := json.Marshal(tcr.StructuredContent)
	require.Contains(t, string(scb), "user_id")
	require.Contains(t, string(scb), "scopes")
}

func TestMCP_ScopeDenied(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{Name: "server.exec", Arguments: jsonRaw(map[string]any{"server_id": 7, "cmd": "echo"})}),
	})
	mcpEndpoint(c)
	_, tcr := decodeRPC(w)
	require.NotNil(t, tcr)
	require.True(t, tcr.IsError)
	require.Contains(t, tcr.Content[0].Text, "missing required scope")
}

func TestMCP_PermissionDenied_WhenWrongUserOwnsServer(t *testing.T) {
	cleanup, _ := setupMCPTest(t)
	defer cleanup()
	require.NoError(t, singleton.DB.Create(&model.User{Common: model.Common{ID: 200}, Username: "bob", Role: model.RoleMember}).Error)
	tok, _ := mkToken(t, 200, []string{model.ScopeServerRead}, nil)
	c, w := mcpCallCtx(t, tok, 200, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{Name: "fs.list", Arguments: jsonRaw(map[string]any{"server_id": 7, "path": "/tmp"})}),
	})
	mcpEndpoint(c)
	_, tcr := decodeRPC(w)
	require.NotNil(t, tcr)
	require.True(t, tcr.IsError)
	require.Contains(t, tcr.Content[0].Text, "permission denied")
}

func TestMCP_ServerWhitelist_DenyOutsideList(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, []uint64{99})
	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{Name: "fs.list", Arguments: jsonRaw(map[string]any{"server_id": 7, "path": "/tmp"})}),
	})
	mcpEndpoint(c)
	_, tcr := decodeRPC(w)
	require.NotNil(t, tcr)
	require.True(t, tcr.IsError)
}

func TestMCP_UnknownTool(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{Name: "does.not.exist", Arguments: json.RawMessage("{}")}),
	})
	mcpEndpoint(c)
	env, _ := decodeRPC(w)
	require.NotNil(t, env.Error)
	require.Equal(t, rpcErrMethodNotFound, env.Error.Code)
}

func TestMCP_InvalidJSONEnvelope(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/mcp", bytes.NewReader([]byte("garbage")))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: uid}, Role: model.RoleMember})
	c.Set(apiTokenCtxKey, tok)
	mcpEndpoint(c)
	var env jsonRPCResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.NotNil(t, env.Error)
	require.Equal(t, rpcErrParse, env.Error.Code)
}

func TestMCP_AuditRowIsWritten(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	c, _ := mcpCallCtx(t, tok, uid, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{Name: "meta.whoami", Arguments: json.RawMessage("{}")}),
	})
	mcpEndpoint(c)

	require.Eventually(t, func() bool {
		var cnt int64
		_ = singleton.DB.Model(&model.MCPAuditLog{}).Where("token_id = ?", tok.ID).Count(&cnt).Error
		return cnt == 1
	}, 2*time.Second, 20*time.Millisecond, "audit row never appeared")
}

func TestMCP_RateLimit(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	original := mcpRateLimiterShared
	mcpRateLimiterShared = newMCPRateLimiter(2, 100)
	defer func() { mcpRateLimiterShared = original }()

	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)

	for i := 0; i < 2; i++ {
		c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
			JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
			Params: jsonObj(t, toolCallParams{Name: "meta.whoami", Arguments: json.RawMessage("{}")}),
		})
		mcpEndpoint(c)
		_, tcr := decodeRPC(w)
		require.False(t, tcr.IsError)
	}
	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{Name: "meta.whoami", Arguments: json.RawMessage("{}")}),
	})
	mcpEndpoint(c)
	_, tcr := decodeRPC(w)
	require.True(t, tcr.IsError)
	require.Contains(t, tcr.Content[0].Text, "rate limit")
}

func jsonObj(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func jsonRaw(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func ctoa(v uint64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
