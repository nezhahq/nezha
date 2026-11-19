package controller

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type bodyObserver struct {
	io.Reader
	onRead func()
}

func (b bodyObserver) Read(p []byte) (int, error) {
	b.onRead()
	return b.Reader.Read(p)
}

func mcpRateLimiterCharged(r *MCPRateLimiter) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	charged := 0
	for _, w := range r.perToken {
		charged += w.minCount
	}
	return charged
}

// The client decides when a body follows its head. A budget unit taken at the
// head lets one token collect allowances over many windows and use them all in
// the same instant, so it must be taken once the body is in.
func TestMCPRateLimitIsChargedAfterTheBodyArrived(t *testing.T) {
	tok := mcpRateLimitTestSetup(t, 2)

	reads := 0
	c, _ := mcpEndpointRawCtx(t, tok, nil)
	c.Request.Body = io.NopCloser(bodyObserver{strings.NewReader("{not valid json"), func() {
		reads++
		require.Zero(t, mcpRateLimiterCharged(mcpRateLimiterShared), "budget charged while the client still withholds the body")
	}})
	mcpEndpoint(c)

	require.NotZero(t, reads)
	require.Equal(t, 1, mcpRateLimiterCharged(mcpRateLimiterShared), "a malformed body must still consume the budget")
}
