package controller

import (
	"net/http"
	"time"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/cmd/dashboard/controller/waf"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

func initParams() *jwt.GinJWTMiddleware {
	return &jwt.GinJWTMiddleware{
		Realm:       singleton.Conf.SiteName,
		Key:         []byte(singleton.Conf.JWTSecretKey),
		CookieName:  "nz-jwt",
		SendCookie:  true,
		Timeout:     time.Hour * time.Duration(singleton.Conf.JWTTimeout),
		MaxRefresh:  time.Hour * time.Duration(singleton.Conf.JWTTimeout),
		IdentityKey: model.CtxKeyAuthorizedUser,
		PayloadFunc: payloadFunc(),

		IdentityHandler: identityHandler(),
		Authenticator:   authenticator(),
		Authorizator:    authorizator(),
		Unauthorized:    unauthorized(),
		TokenLookup:     "header: Authorization, query: token, cookie: nz-jwt",
		TokenHeadName:   "Bearer",
		TimeFunc:        time.Now,

		LoginResponse: func(c *gin.Context, code int, token string, expire time.Time) {
			c.JSON(http.StatusOK, model.CommonResponse[model.LoginResponse]{
				Success: true,
				Data: model.LoginResponse{
					Token:  token,
					Expire: expire.Format(time.RFC3339),
				},
			})
		},
		RefreshResponse: refreshResponse,
	}
}

func payloadFunc() func(data any) jwt.MapClaims {
	return func(data any) jwt.MapClaims {
		if v, ok := data.(map[string]interface{}); ok {
			return v
		}
		return jwt.MapClaims{}
	}
}

func identityHandler() func(c *gin.Context) any {
	return func(c *gin.Context) any {
		claims := jwt.ExtractClaims(c)

		userId, ok := claims["user_id"].(string)
		if !ok {
			return nil
		}

		tokenIP, ok := claims["ip"].(string)
		if !ok {
			return nil
		}

		currentIP := c.GetString(model.CtxKeyRealIPStr)

		if tokenIP != currentIP {
			// IP地址不匹配，token无效
			c.Set(model.CtxKeyIsIPMismatch, true)
			return nil
		}

		var user model.User
		if err := singleton.DB.First(&user, userId).Error; err != nil {
			return nil
		}
		return &user
	}
}

// User Login
// @Summary user login
// @Schemes
// @Description user login
// @Accept json
// @param loginRequest body model.LoginRequest true "Login Request"
// @Produce json
// @Success 200 {object} model.CommonResponse[model.LoginResponse]
// @Router /login [post]
func authenticator() func(c *gin.Context) (any, error) {
	return func(c *gin.Context) (any, error) {
		var loginVals model.LoginRequest
		if err := c.ShouldBind(&loginVals); err != nil {
			return "", jwt.ErrMissingLoginValues
		}

		var user model.User
		realip := c.GetString(model.CtxKeyRealIPStr)

		if err := singleton.DB.Select("id", "password", "reject_password").Where("username = ?", loginVals.Username).First(&user).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				model.BlockIP(singleton.DB, realip, model.WAFBlockReasonTypeLoginFail, model.BlockIDUnknownUser)
			}
			return nil, jwt.ErrFailedAuthentication
		}

		if user.RejectPassword {
			model.BlockIP(singleton.DB, realip, model.WAFBlockReasonTypeLoginFail, int64(user.ID))
			return nil, jwt.ErrFailedAuthentication
		}

		if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(loginVals.Password)); err != nil {
			model.BlockIP(singleton.DB, realip, model.WAFBlockReasonTypeLoginFail, int64(user.ID))
			return nil, jwt.ErrFailedAuthentication
		}

		model.UnblockIP(singleton.DB, realip, model.BlockIDUnknownUser)
		model.UnblockIP(singleton.DB, realip, int64(user.ID))

		// 返回用户ID和IP地址的组合，用于在payloadFunc中设置JWT claims
		return map[string]interface{}{
			"user_id": utils.Itoa(user.ID),
			"ip":      realip,
		}, nil
	}
}

func authorizator() func(data any, c *gin.Context) bool {
	return func(data any, c *gin.Context) bool {
		_, ok := data.(*model.User)
		return ok
	}
}

func unauthorized() func(c *gin.Context, code int, message string) {
	return func(c *gin.Context, code int, message string) {
		c.JSON(http.StatusOK, model.CommonResponse[any]{
			Success: false,
			Error:   "ApiErrorUnauthorized",
		})
	}
}

// Refresh token
// @Summary Refresh token
// @Security BearerAuth
// @Schemes
// @Description Refresh token
// @Tags auth required
// @Produce json
// @Success 200 {object} model.CommonResponse[model.LoginResponse]
// @Router /refresh-token [get]
func refreshResponse(c *gin.Context, code int, token string, expire time.Time) {
	c.JSON(http.StatusOK, model.CommonResponse[model.LoginResponse]{
		Success: true,
		Data: model.LoginResponse{
			Token:  token,
			Expire: expire.Format(time.RFC3339),
		},
	})
}

func fallbackAuthMiddleware(mw *jwt.GinJWTMiddleware) func(c *gin.Context) {
	return func(c *gin.Context) {
		claims, err := mw.GetClaimsFromJWT(c)
		if err != nil {
			return
		}

		switch v := claims["exp"].(type) {
		case nil:
			return
		case float64:
			if int64(v) < mw.TimeFunc().Unix() {
				return
			}
		case json.Number:
			n, err := v.Int64()
			if err != nil {
				return
			}
			if n < mw.TimeFunc().Unix() {
				return
			}
		default:
			return
		}

		realIP := c.GetString(model.CtxKeyRealIPStr)

		c.Set("JWT_PAYLOAD", claims)
		identity := mw.IdentityHandler(c)

		if identity != nil {
			model.UnblockIP(singleton.DB, realIP, model.BlockIDToken)
			c.Set(mw.IdentityKey, identity)
		} else {
			isIpMismatch := c.GetBool(model.CtxKeyIsIPMismatch)
			if !isIpMismatch {
				waf.ShowBlockPage(c, model.BlockIP(singleton.DB, realIP, model.WAFBlockReasonTypeBruteForceToken, model.BlockIDToken))
				return
			}
		}

		c.Next()
	}
}
