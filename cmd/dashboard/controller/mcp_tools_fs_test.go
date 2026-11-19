package controller

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// fs.* 跨租户拒绝测试：member token 调 fs.list/read/write/delete 时，如果
// server.UserID != caller.ID 必须 isError 返回，且不会触达 agent。
//
// 这些用例不依赖 agent simulator —— 它们要验证的就是 requireServerAccess 在
// agent 调用前拦截。如果错误发生在 agent CallAgent，说明权限漏失。

func makeForeignServerMCP(t *testing.T, id, ownerUID uint64) {
	t.Helper()
	srv := &model.Server{}
	srv.ID = id
	srv.SetUserID(ownerUID)
	singleton.ServerShared.InsertForTest(srv)
}

func TestMCPFs_List_ForeignServerRejected(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	makeForeignServerMCP(t, 200, 999)

	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{
			Name:      "fs.list",
			Arguments: jsonRaw(map[string]any{"server_id": 200, "path": "/etc"}),
		}),
	})
	mcpEndpoint(c)
	_, tcr := decodeRPC(w)
	require.NotNil(t, tcr)
	require.True(t, tcr.IsError)
	require.Contains(t, tcr.Content[0].Text, "permission denied")
}

func TestMCPFs_Read_ForeignServerRejected(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	makeForeignServerMCP(t, 201, 999)

	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{
			Name:      "fs.read",
			Arguments: jsonRaw(map[string]any{"server_id": 201, "path": "/etc/passwd"}),
		}),
	})
	mcpEndpoint(c)
	_, tcr := decodeRPC(w)
	require.True(t, tcr.IsError)
	require.Contains(t, tcr.Content[0].Text, "permission denied")
}

func TestMCPFs_Write_ForeignServerRejected(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	makeForeignServerMCP(t, 202, 999)

	tok, _ := mkToken(t, uid, []string{model.ScopeServerWrite}, nil)
	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{
			Name: "fs.write",
			Arguments: jsonRaw(map[string]any{
				"server_id": 202, "path": "/tmp/evil", "content": "x",
			}),
		}),
	})
	mcpEndpoint(c)
	_, tcr := decodeRPC(w)
	require.True(t, tcr.IsError)
	require.Contains(t, tcr.Content[0].Text, "permission denied")
}

func TestMCPFs_Delete_ForeignServerRejected(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	makeForeignServerMCP(t, 203, 999)

	tok, _ := mkToken(t, uid, []string{model.ScopeServerDelete}, nil)
	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{
			Name:      "fs.delete",
			Arguments: jsonRaw(map[string]any{"server_id": 203, "path": "/tmp/foo"}),
		}),
	})
	mcpEndpoint(c)
	_, tcr := decodeRPC(w)
	require.True(t, tcr.IsError)
	require.Contains(t, tcr.Content[0].Text, "permission denied")
}

func TestMCPFs_DownloadURL_ForeignServerRejected(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	makeForeignServerMCP(t, 204, 999)

	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{
			Name:      "fs.download_url",
			Arguments: jsonRaw(map[string]any{"server_id": 204, "path": "/etc/shadow"}),
		}),
	})
	mcpEndpoint(c)
	_, tcr := decodeRPC(w)
	require.True(t, tcr.IsError)
	require.Contains(t, tcr.Content[0].Text, "permission denied")
}

func TestMCPFs_UploadURL_ForeignServerRejected(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	makeForeignServerMCP(t, 205, 999)

	tok, _ := mkToken(t, uid, []string{model.ScopeServerWrite}, nil)
	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{
			Name:      "fs.upload_url",
			Arguments: jsonRaw(map[string]any{"server_id": 205, "path": "/tmp/up"}),
		}),
	})
	mcpEndpoint(c)
	_, tcr := decodeRPC(w)
	require.True(t, tcr.IsError)
	require.Contains(t, tcr.Content[0].Text, "permission denied")
}

func TestMCPFs_PATServerWhitelistFiltersFs(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	// Both servers owned by the same user, but the PAT only whitelists server 300.
	// fs.list against server 7 (in setupMCPTest) must be denied even though
	// caller user owns it, because the PAT was minted for server 300 only.
	srv := &model.Server{}
	srv.ID = 300
	srv.SetUserID(uid)
	singleton.ServerShared.InsertForTest(srv)

	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, []uint64{300})

	c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
		Params: jsonObj(t, toolCallParams{
			Name:      "fs.list",
			Arguments: jsonRaw(map[string]any{"server_id": 7, "path": "/etc"}),
		}),
	})
	mcpEndpoint(c)
	_, tcr := decodeRPC(w)
	require.True(t, tcr.IsError)
	require.Contains(t, tcr.Content[0].Text, "permission denied")
}

// fs.* and server.exec arguments are re-encoded into Task.Data, so their size
// must be bounded before CallAgent, like fs.download_url / fs.upload_url already
// bound the path (fs.upload_url keeps its mode in the transfer entry and forwards
// it the same way). Server 7 has no task stream: a call that got past the bounds
// would answer "agent offline" instead.
func TestMCPAgentCall_OversizedArgumentsRejectedBeforeDispatch(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()

	longPath := strings.Repeat("<", maxTransferPathLen+1)
	longOption := strings.Repeat("<", maxFsOptionLen+1)
	bulk := strings.Repeat("<", mcpMaxInlineArgBytes+1)
	emptyArgs := make([]string, mcpMaxInlineArgBytes+1)
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"fs.list", map[string]any{"server_id": 7, "path": longPath}, "path too long"},
		{"fs.read", map[string]any{"server_id": 7, "path": longPath}, "path too long"},
		{"fs.read", map[string]any{"server_id": 7, "path": "/tmp/x", "encoding": longOption}, "encoding too long"},
		{"fs.write", map[string]any{"server_id": 7, "path": longPath, "content": "x"}, "path too long"},
		{"fs.write", map[string]any{"server_id": 7, "path": "/tmp/x", "content": bulk}, "content too large"},
		{"fs.write", map[string]any{"server_id": 7, "path": "/tmp/x", "content": "x", "encoding": longOption}, "encoding too long"},
		{"fs.write", map[string]any{"server_id": 7, "path": "/tmp/x", "content": "x", "mode": longOption}, "mode too long"},
		{"fs.delete", map[string]any{"server_id": 7, "path": longPath}, "path too long"},
		{"fs.upload_url", map[string]any{"server_id": 7, "path": "/tmp/x", "mode": longOption}, "mode too long"},
		{"server.exec", map[string]any{"server_id": 7, "cmd": bulk}, "too large"},
		{"server.exec", map[string]any{"server_id": 7, "cmd": "sh", "cwd": bulk}, "too large"},
		{"server.exec", map[string]any{"server_id": 7, "cmd": "sh", "stdin": bulk}, "too large"},
		{"server.exec", map[string]any{"server_id": 7, "cmd": "sh", "args": []string{bulk}}, "too large"},
		{"server.exec", map[string]any{"server_id": 7, "cmd": "sh", "args": emptyArgs}, "too large"},
		{"server.exec", map[string]any{"server_id": 7, "cmd": "sh", "env": map[string]string{"K": bulk}}, "too large"},
		// Arguments within the bounds still reach the agent call.
		{"fs.list", map[string]any{"server_id": 7, "path": strings.Repeat("a", maxTransferPathLen)}, "agent offline"},
		{"fs.write", map[string]any{"server_id": 7, "path": "/tmp/x", "content": bulk[1:]}, "agent offline"},
		{"server.exec", map[string]any{"server_id": 7, "cmd": "sh", "stdin": bulk[3:]}, "agent offline"},
	}
	tok, _ := mkToken(t, uid, []string{"nezha:server:*"}, nil)
	for _, tc := range cases {
		c, w := mcpCallCtx(t, tok, uid, jsonRPCRequest{
			JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
			Params: jsonObj(t, toolCallParams{Name: tc.tool, Arguments: jsonRaw(tc.args)}),
		})
		mcpEndpoint(c)
		_, tcr := decodeRPC(w)
		require.NotNil(t, tcr, tc.tool)
		require.True(t, tcr.IsError, tc.tool)
		require.Contains(t, tcr.Content[0].Text, tc.want, tc.tool)
	}
}
