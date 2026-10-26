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
