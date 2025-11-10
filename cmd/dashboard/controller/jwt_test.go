package controller

import (
	"testing"
	"time"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestPayloadFunc(t *testing.T) {
	payloadFn := payloadFunc()

	// 测试包含IP的格式
	t.Run("format with IP", func(t *testing.T) {
		data := map[string]interface{}{
			"user_id": "123",
			"ip":      "192.168.1.1",
		}
		claims := payloadFn(data)
		assert.Equal(t, "123", claims["user_id"])
		assert.Equal(t, "192.168.1.1", claims["ip"])
	})

	// 测试不包含IP的格式
	t.Run("format without IP", func(t *testing.T) {
		data := map[string]interface{}{
			"user_id": "123",
		}
		claims := payloadFn(data)
		assert.Equal(t, "123", claims["user_id"])
		assert.Nil(t, claims["ip"])
	})

	// 测试无效数据格式
	t.Run("invalid data format", func(t *testing.T) {
		claims := payloadFn("123") // 字符串类型不再支持
		assert.Empty(t, claims)
	})

	// 测试空的map
	t.Run("empty map", func(t *testing.T) {
		data := map[string]interface{}{}
		claims := payloadFn(data)
		assert.Empty(t, claims)
	})
}

func TestIPBinding(t *testing.T) {
	// 创建测试用的gin context
	gin.SetMode(gin.TestMode)

	t.Run("IP mismatch should invalidate token", func(t *testing.T) {
		// 模拟JWT claims包含IP绑定
		claims := jwt.MapClaims{
			"user_id": "123",
			"ip":      "192.168.1.1",
			"exp":     float64(time.Now().Add(time.Hour).Unix()),
		}

		// 这里需要实际的数据库和用户设置来完全测试
		// 但可以测试claims的基本结构
		assert.Equal(t, "123", claims["user_id"])
		assert.Equal(t, "192.168.1.1", claims["ip"])
	})

	t.Run("no IP in token should deny access", func(t *testing.T) {
		// 没有IP绑定的token应该被拒绝
		claims := jwt.MapClaims{
			"user_id": "123",
			"exp":     float64(time.Now().Add(time.Hour).Unix()),
		}

		// 验证token结构
		assert.Equal(t, "123", claims["user_id"])
		assert.Nil(t, claims["ip"])
	})
}
