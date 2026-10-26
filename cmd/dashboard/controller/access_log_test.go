package controller

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccessLogOmitsCredentialURLs(t *testing.T) {
	var output bytes.Buffer
	original := gin.DefaultWriter
	gin.DefaultWriter = &output
	defer func() { gin.DefaultWriter = original }()
	router := gin.New()
	router.Use(dashboardRequestLogger())
	router.GET("/ws", func(c *gin.Context) { c.Status(204) })
	router.GET("/mcp/download/:token", func(c *gin.Context) { c.Status(204) })
	for _, path := range []string{"/ws?token=synthetic-session-credential", "/mcp/download/synthetic-transfer-credential?query=sensitive"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	require.NotContains(t, output.String(), "synthetic-session-credential")
	require.NotContains(t, output.String(), "synthetic-transfer-credential")
	require.NotContains(t, output.String(), "query=sensitive")
	require.Contains(t, output.String(), "/ws")
	require.Contains(t, output.String(), "/mcp/download/")
}
