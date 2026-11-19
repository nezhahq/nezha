package controller

import (
	"bytes"
	"log"
	"net/http/httptest"
	"strings"
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

func TestAccessLogBoundsClientChosenFields(t *testing.T) {
	var output bytes.Buffer
	original := gin.DefaultWriter
	gin.DefaultWriter = &output
	defer func() { gin.DefaultWriter = original }()
	router := gin.New()
	router.Use(dashboardRequestLogger())

	// Ordinary lines keep their format.
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/server", nil))
	require.True(t, strings.HasSuffix(output.String(), "| GET \"/api/v1/server\"\n"), output.String())

	// %q writes each byte from 0x80 up as four, so the bound has to hold for the quoted form.
	for _, path := range []string{"/api" + strings.Repeat("a", 1<<20), "/api" + strings.Repeat("\x80", 1<<20), "/" + strings.Repeat("\x80", maxLoggedPathBytes-1)} {
		output.Reset()
		request := httptest.NewRequest("GET", "/", nil)
		request.URL.Path = path
		router.ServeHTTP(httptest.NewRecorder(), request)
		require.Less(t, output.Len(), 2*maxLoggedPathBytes)
		require.Equal(t, 1, strings.Count(output.String(), "\n"))
		require.Contains(t, output.String(), "| GET \"/")
	}

	// net/http does not check that an HTTP/2 :method is a token.
	for _, method := range []string{strings.Repeat("X", 1<<20), `GET "/forged"`} {
		output.Reset()
		request := httptest.NewRequest("GET", "/", nil)
		request.Method = method
		router.ServeHTTP(httptest.NewRecorder(), request)
		require.Less(t, output.Len(), 2*maxLoggedPathBytes)
		require.Contains(t, output.String(), "[invalid method]")
		require.NotContains(t, output.String(), "forged")
	}
}

func TestRecoveryLogBoundsClientChosenFields(t *testing.T) {
	var output bytes.Buffer
	original := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(original)
	router := gin.New()
	router.Use(dashboardRecovery())
	router.NoRoute(func(c *gin.Context) { panic("synthetic") })

	request := httptest.NewRequest("GET", "/", nil)
	request.URL.Path = "/api" + strings.Repeat("\x80", 1<<20)
	request.Method = strings.Repeat("X", 1<<20)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, 500, recorder.Code)
	require.Less(t, output.Len(), 2*maxLoggedPathBytes)
}
