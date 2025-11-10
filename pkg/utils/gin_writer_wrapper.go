package utils

import "github.com/gin-gonic/gin"

type GinCustomWriter struct {
	gin.ResponseWriter

	customCode int
}

func NewGinCustomWriter(c *gin.Context, code int) *GinCustomWriter {
	return &GinCustomWriter{
		ResponseWriter: c.Writer,
		customCode:     code,
	}
}

func (w *GinCustomWriter) WriteHeader(code int) {
	w.ResponseWriter.WriteHeader(w.customCode)
}
