package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// fs.upload / fs.download 失败路径必须写一条 MCPAuditLog，否则审计表只能看到
// 成功调用，运营无法发现"PAT 被吊销后仍有人尝试消费 URL"、"agent 拒绝执行"、
// "kill switch 已开却仍有调用打进来"这类信号。成功路径已经在写审计，这里把
// 失败路径的契约钉死。

func countAuditRows(t *testing.T, tool, outcome string) int64 {
	t.Helper()
	var cnt int64
	q := singleton.DB.Model(&model.MCPAuditLog{}).Where("tool = ?", tool)
	if outcome != "" {
		q = q.Where("outcome = ?", outcome)
	}
	require.NoError(t, q.Count(&cnt).Error)
	return cnt
}

func newTransferRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/mcp/download/:token", transferDownloadHandler)
	r.POST("/mcp/upload/:token", transferUploadHandler)
	return r
}

func TestTransferDownload_AuditsTokenExpired(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	r := newTransferRouter(t)

	url, err := mintTransferToken(transferEntry{
		UserID:    uid,
		TokenID:   tok.ID,
		ServerID:  7,
		Path:      "/srv/blob",
		Direction: transferDirDownload,
		ExpiresAt: time.Now().Add(-time.Second),
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/mcp/download/"+url, nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code,
		"expired token must surface as 401 to client")
	require.Equal(t, int64(1), countAuditRows(t, "fs.download", ""),
		"failed download must still produce an audit row so SIEM can observe the rejection")
}

func TestTransferDownload_RejectsWithoutAuditWhenMCPDisabled(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	r := newTransferRouter(t)

	url, err := mintTransferToken(transferEntry{
		UserID:    uid,
		TokenID:   tok.ID,
		ServerID:  7,
		Path:      "/srv/blob",
		Direction: transferDirDownload,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	require.NoError(t, err)
	singleton.Conf.SetMCPEnabled(false)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/mcp/download/"+url, nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
	require.Equal(t, int64(0),
		countAuditRows(t, "fs.download", model.MCPOutcomeMCPDisabled),
		"disabled MCP must not let anonymous traffic grow the audit database")
}

func TestTransferUpload_AuditsTokenExpired(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerWrite}, nil)
	r := newTransferRouter(t)

	url, err := mintTransferToken(transferEntry{
		UserID:    uid,
		TokenID:   tok.ID,
		ServerID:  7,
		Path:      "/srv/blob",
		Direction: transferDirUpload,
		ExpiresAt: time.Now().Add(-time.Second),
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/mcp/upload/"+url, strings.NewReader(""))
	req.ContentLength = 0
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, int64(1), countAuditRows(t, "fs.upload", ""),
		"failed upload must still produce an audit row")
}

func TestTransferUpload_RejectsWithoutAuditWhenMCPDisabled(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerWrite}, nil)
	r := newTransferRouter(t)

	url, err := mintTransferToken(transferEntry{
		UserID:    uid,
		TokenID:   tok.ID,
		ServerID:  7,
		Path:      "/srv/blob",
		Direction: transferDirUpload,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	require.NoError(t, err)
	singleton.Conf.SetMCPEnabled(false)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/mcp/upload/"+url, strings.NewReader(""))
	req.ContentLength = 0
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)
	require.Equal(t, int64(0),
		countAuditRows(t, "fs.upload", model.MCPOutcomeMCPDisabled),
		"disabled uploads must not allocate audit rows")
}

// A URL refused for its direction or expiry is genuine: consumeTransferToken
// has its entry and has verified its signature. The refusal must be audited
// against the token the URL was minted from, outside the anonymous sampler
// (5 rows per minute and address), not as a guess with user/token/server 0.
func TestTransferRefusedGenuineURLIsAuditedForItsToken(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	r := newTransferRouter(t)

	refuse := func(expiresAt time.Time, method, route, wantBody string) {
		t.Helper()
		url, err := mintTransferToken(transferEntry{
			UserID:    uid,
			TokenID:   tok.ID,
			ServerID:  7,
			Path:      "/srv/blob",
			Direction: transferDirDownload,
			ExpiresAt: expiresAt,
		})
		require.NoError(t, err)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, route+url, strings.NewReader(""))
		req.ContentLength = 0
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.Equal(t, wantBody, w.Body.String())
		_, err = consumeTransferToken(url, transferDirDownload)
		require.Error(t, err, "a refused URL must stay consumed")
	}
	const each = 8 // more than the anonymous per-address budget
	for range each {
		refuse(time.Now().Add(time.Minute), "POST", "/mcp/upload/", "transfer token direction mismatch")
		refuse(time.Now().Add(-time.Second), "GET", "/mcp/download/", "transfer token expired")
	}

	attributed := func(tool, outcome string) int64 {
		t.Helper()
		var cnt int64
		require.NoError(t, singleton.DB.Model(&model.MCPAuditLog{}).
			Where("tool = ? AND outcome = ? AND user_id = ? AND token_id = ? AND server_id = ?", tool, outcome, uid, tok.ID, 7).
			Count(&cnt).Error)
		return cnt
	}
	require.Equal(t, int64(each), attributed("fs.upload", model.MCPOutcomeInvalidArgs),
		"every wrong-direction refusal must name the token of the URL")
	require.Equal(t, int64(each), attributed("fs.download", model.MCPOutcomeScopeDenied),
		"every expiry refusal must name the token of the URL")
}
