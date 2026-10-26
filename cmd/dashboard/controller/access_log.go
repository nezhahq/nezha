package controller

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"log"
	"net/http"
	"net/url"
	"strings"
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

func dashboardRequestLogger() gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(entry gin.LogFormatterParams) string {
		return fmt.Sprintf("[GIN] %s | %d | %s | %q | %s %q\n", entry.TimeStamp.Format("2006/01/02 15:04:05"), entry.StatusCode, entry.Latency, entry.ClientIP, entry.Method, credentialFreeRequestPath(entry.Path))
	})
}

func dashboardRecovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recover() != nil {
				// Gin's default recovery dumps the request, including cookie and URL credentials.
				log.Printf("NEZHA>> recovered request panic: %s %q", c.Request.Method, credentialFreeRequestPath(c.Request.URL.RequestURI()))
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}
