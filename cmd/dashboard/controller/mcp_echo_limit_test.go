package controller

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The id, method and tool name of a request are attacker-sized (up to
// mcpJSONRPCMaxBodyBytes) and the JSON encoder writes '<' as six bytes, so
// none of them may be repeated in a reply beyond a small bound, whatever the
// outcome, rate-limit refusals included. Otherwise a few concurrent 8 MiB
// pings from any valid token exhaust the dashboard's memory.
func TestMCPEndpoint_OversizedRequestTextIsNotEchoed(t *testing.T) {
	filler := strings.Repeat("<", 64*1024)
	cases := map[string]string{
		"id":              `{"jsonrpc":"2.0","id":"` + filler + `","method":"ping"}`,
		"id, bad version": `{"jsonrpc":"1.0","id":"` + filler + `","method":"ping"}`,
		"id, tools/call":  `{"jsonrpc":"2.0","id":"` + filler + `","method":"tools/call","params":{"name":"meta.whoami"}}`,
		"method":          `{"jsonrpc":"2.0","id":1,"method":"` + filler + `"}`,
		"tool name":       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + filler + `"}}`,
	}
	for name, raw := range cases {
		for _, overBudget := range []bool{false, true} {
			tok := mcpRateLimitTestSetup(t, 1)
			if overBudget {
				require.True(t, mcpRateLimiterShared.Allow(tok.ID))
			}
			c, w := mcpEndpointRawCtx(t, tok, []byte(raw))
			mcpEndpoint(c)
			require.Less(t, w.Body.Len(), 2048, "%s (over budget: %v)", name, overBudget)
			require.NotContains(t, w.Body.String(), "<<", "%s (over budget: %v)", name, overBudget)
			if strings.HasPrefix(name, "id") {
				// Refused as an envelope error without an id; the limiter's
				// verdict still takes precedence.
				require.Contains(t, w.Body.String(), `"error"`, name)
				require.NotContains(t, w.Body.String(), `"id"`, name)
				if overBudget {
					require.Equal(t, http.StatusTooManyRequests, w.Code, name)
				}
			}
		}
	}
}

// An id within the bound is still echoed verbatim.
func TestMCPEndpoint_OrdinaryIDIsEchoed(t *testing.T) {
	tok := mcpRateLimitTestSetup(t, 10)
	id := `"` + strings.Repeat("a", mcpJSONRPCMaxIDBytes-2) + `"`
	c, w := mcpEndpointRawCtx(t, tok, []byte(`{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`))
	mcpEndpoint(c)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"jsonrpc":"2.0","id":`+id+`,"result":{}}`, w.Body.String())
}

// Tool handlers surface decodeToolArgs errors to the caller; encoding/json
// quotes a number literal that does not fit the target field in full.
func TestDecodeToolArgs_ErrorDoesNotEchoOversizedLiteral(t *testing.T) {
	var args struct {
		ServerID uint64 `json:"server_id"`
	}
	err := decodeToolArgs(json.RawMessage(`{"server_id":`+strings.Repeat("1", 64*1024)+`}`), &args)
	require.Error(t, err)
	require.Less(t, len(err.Error()), 1024)
	require.Contains(t, err.Error(), "invalid arguments: ")
}
