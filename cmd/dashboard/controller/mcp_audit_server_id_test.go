package controller

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// server_id 由调用方填写，不使用它的 tool 也会被记进审计行。database/sql 绑定不了
// 高位为 1 的 uint64，这类值不能让调用（无论成功还是被拒）整条丢掉审计行，否则
// 持 token 的一方就能按次决定自己是否出现在 mcp_audit_logs 里。
func TestMCPAuditRowSurvivesUnbindableServerID(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeInventoryRead}, nil)

	serverIDs := []uint64{math.MaxInt64, math.MaxInt64 + 1, math.MaxUint64}
	calls := []struct {
		tool    string
		outcome string
	}{
		{"server.list", model.MCPOutcomeOK},
		{"meta.whoami", model.MCPOutcomeOK},
		{"server.exec", model.MCPOutcomeScopeDenied},
	}
	for _, serverID := range serverIDs {
		arguments, err := json.Marshal(map[string]uint64{"server_id": serverID})
		require.NoError(t, err)
		for _, call := range calls {
			c, w := mcpCallCtx(t, tok, uid, nil)
			handleToolsCall(c, &jsonRPCRequest{
				JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
				Params: jsonObj(t, toolCallParams{Name: call.tool, Arguments: arguments}),
			}, tok)
			_, result := decodeRPC(w)
			require.NotNil(t, result, call.tool)
			require.Equal(t, call.outcome != model.MCPOutcomeOK, result.IsError, call.tool)
		}
	}

	var logs []model.MCPAuditLog
	require.NoError(t, singleton.DB.Order("id").Find(&logs).Error)
	require.Len(t, logs, len(serverIDs)*len(calls), "every tools/call must leave an audit row")
	for i, entry := range logs {
		serverID, call := serverIDs[i/len(calls)], calls[i%len(calls)]
		require.Equal(t, call.tool, entry.Tool)
		require.Equal(t, call.outcome, entry.Outcome)
		if serverID > math.MaxInt64 {
			serverID = 0
		}
		require.Equal(t, serverID, entry.ServerID, call.tool)
	}
}

func TestExtractServerID(t *testing.T) {
	for raw, want := range map[string]uint64{
		``:                                   0,
		`{}`:                                 0,
		`{"server_id":7}`:                    7,
		`{"server_id":9223372036854775807}`:  math.MaxInt64,
		`{"server_id":9223372036854775808}`:  0,
		`{"server_id":18446744073709551615}`: 0,
		`{"server_id":18446744073709551616}`: 0,
		`{"server_id":-1}`:                   0,
		`{"server_id":"7"}`:                  0,
	} {
		require.Equal(t, want, extractServerID(json.RawMessage(raw)), raw)
	}
}
