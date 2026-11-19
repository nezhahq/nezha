package controller

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"golang.org/x/net/http/httpguts"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The request path and method are chosen by the client, net/http accepts about
// 1 MiB of each, and an HTTP/2 client repeats a stored header for one byte per
// request. Bound what one request can append to the log.
const (
	maxLoggedPathBytes   = 256
	maxLoggedMethodBytes = 32
)

func credentialFreeRequestPath(path string) string {
	parsed, err := url.ParseRequestURI(path)
	if err != nil {
		return "[invalid request path]"
	}
	path = parsed.Path // Never log query values: JWT, OAuth codes and state can all appear here.
	for _, prefix := range []string{"/mcp/download/", "/mcp/upload/"} {
		if strings.HasPrefix(path, prefix) {
			return prefix + "[redacted]"
		}
	}
	return path
}

// loggedRequestPath quotes the credential-free path like %q and keeps at most
// maxLoggedPathBytes of the quoted form. The bound is on the output because
// quoting writes up to four bytes for one.
func loggedRequestPath(path string) string {
	path = credentialFreeRequestPath(path)
	quoted := strconv.Quote(truncateString(path, maxLoggedPathBytes))
	if len(path) <= maxLoggedPathBytes && len(quoted) <= maxLoggedPathBytes+2 {
		return quoted
	}
	return fmt.Sprintf(`%s..." (%d bytes)`, quoted[:maxLoggedPathBytes], len(path))
}

// loggedRequestMethod returns method if it is a short token, which is what
// net/http requires of an HTTP/1 method. An HTTP/2 :method reaches the handlers
// without that check, and is written to the log unquoted.
func loggedRequestMethod(method string) string {
	if len(method) > maxLoggedMethodBytes || strings.ContainsFunc(method, func(r rune) bool { return !httpguts.IsTokenRune(r) }) {
		return "[invalid method]"
	}
	return method
}

func dashboardRequestLogger() gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(entry gin.LogFormatterParams) string {
		return fmt.Sprintf("[GIN] %s | %d | %s | %q | %s %s\n", entry.TimeStamp.Format("2006/01/02 15:04:05"), entry.StatusCode, entry.Latency, entry.ClientIP, loggedRequestMethod(entry.Method), loggedRequestPath(entry.Path))
	})
}

func dashboardRecovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recover() != nil {
				// Gin's default recovery dumps the request, including cookie and URL credentials.
				log.Printf("NEZHA>> recovered request panic: %s %s", loggedRequestMethod(c.Request.Method), loggedRequestPath(c.Request.URL.RequestURI()))
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}
