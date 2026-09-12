package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/cmd/dashboard/controller/waf"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/idcodec"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

const (
	jwtClaimUserID = "uid"
	jwtClaimKeyID  = "keyId"
	jwtKeyIDBytes  = 32
)

func uaHash(c *gin.Context) string {
	sum := sha256.Sum256([]byte(c.Request.UserAgent()))
	return hex.EncodeToString(sum[:])
}

func issueJWTSession(c *gin.Context, user *model.User, jwtTimeoutHours int) (map[string]interface{}, error) {
	keyID, err := utils.GenerateRandomString(jwtKeyIDBytes)
	if err != nil {
		return nil, err
	}
	// encodedUID is reversible Sqids obfuscation keyed by JWTSecretKey, NOT
	// a one-way hash. It exists to defeat enumeration on the wire, not to
	// keep the uid confidential — see L2 note in idcodec docs.
	encodedUID, err := idcodec.Encode(user.ID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	sess := model.JWTSession{
		KeyID:        keyID,
		UserID:       user.ID,
		IP:           c.GetString(model.CtxKeyRealIPStr),
		UAHash:       uaHash(c),
		TokenVersion: user.TokenVersion,
		ExpiresAt:    now.Add(time.Hour * time.Duration(jwtTimeoutHours)),
		CreatedAt:    now,
		LastUsedAt:   now,
	}
	if err := singleton.DB.Create(&sess).Error; err != nil {
		return nil, err
	}
	return map[string]interface{}{
		jwtClaimUserID: encodedUID,
		jwtClaimKeyID:  keyID,
	}, nil
}

func initParams() *jwt.GinJWTMiddleware {
	return &jwt.GinJWTMiddleware{
		Realm:      singleton.Conf.SiteName,
		Key:        []byte(singleton.Conf.JWTSecretKey),
		CookieName: "nz-jwt",
		SendCookie: true,
		// Pin the signing algorithm so a future library default change (or an
		// `alg: none` confusion attempt) cannot weaken token validation.
		SigningAlgorithm: "HS256",
		// Lax keeps OAuth callback redirects (top-level GET navigations from
		// the provider domain) working while blocking cross-site POST CSRF.
		// HttpOnly/Secure are intentionally left default: the frontend reads
		// `!!document.cookie` for login-state display and many deployments
		// terminate TLS at a proxy upstream — both warrant a separate change.
		CookieSameSite: http.SameSiteLaxMode,
		Timeout:        time.Hour * time.Duration(singleton.Conf.JWTTimeout),
		MaxRefresh:     time.Hour * time.Duration(singleton.Conf.JWTTimeout),
		IdentityKey:    model.CtxKeyAuthorizedUser,
		PayloadFunc:    payloadFunc(),

		IdentityHandler: identityHandler(),
		Authenticator:   authenticator(),
		Authorizator:    authorizator(),
		Unauthorized:    unauthorized(),
		// query: token still accepted because the WebSocket browser API
		// cannot set Authorization headers; removing it would break the
		// /ws/* routes until the frontend migrates to cookie auth.
		TokenLookup:   "header: Authorization, query: token, cookie: nz-jwt",
		TokenHeadName: "Bearer",
		TimeFunc:      time.Now,

		LoginResponse: func(c *gin.Context, code int, token string, expire time.Time) {
			setCSRFCookie(c)
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

		keyID, ok := claims[jwtClaimKeyID].(string)
		if !ok || keyID == "" {
			return nil
		}
		encodedUID, ok := claims[jwtClaimUserID].(string)
		if !ok || encodedUID == "" {
			return nil
		}
		claimUID, err := idcodec.Decode(encodedUID)
		if err != nil {
			realIP := c.GetString(model.CtxKeyRealIPStr)
			model.BlockIP(singleton.DB, realIP, model.WAFBlockReasonTypeBruteForceToken, model.BlockIDToken)
			return nil
		}

		var sess model.JWTSession
		if err := singleton.DB.First(&sess, "key_id = ?", keyID).Error; err != nil {
			return nil
		}
		if sess.RevokedAt != nil {
			return nil
		}
		now := time.Now()
		if now.After(sess.ExpiresAt) {
			return nil
		}
		if claimUID != sess.UserID {
			realIP := c.GetString(model.CtxKeyRealIPStr)
			model.BlockIP(singleton.DB, realIP, model.WAFBlockReasonTypeBruteForceToken, model.BlockIDToken)
			return nil
		}
		currentIP := c.GetString(model.CtxKeyRealIPStr)
		if !singleton.Conf.JWTIPChangeAllowed() && sess.IP != currentIP {
			c.Set(model.CtxKeyIsIPMismatch, true)
			return nil
		}

		var user model.User
		if err := singleton.DB.First(&user, sess.UserID).Error; err != nil {
			return nil
		}
		if user.TokenVersion != sess.TokenVersion {
			return nil
		}

		_ = singleton.DB.Model(&model.JWTSession{}).
			Where("key_id = ?", keyID).
			Update("last_used_at", now).Error

		c.Set(jwtClaimKeyID, keyID)
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

		if err := singleton.DB.Select("id", "password", "reject_password", "token_version").Where("username = ?", loginVals.Username).First(&user).Error; err != nil {
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

		return issueJWTSession(c, &user, singleton.Conf.JWTTimeout)
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
// @Router /refresh-token [post]
func refreshResponse(c *gin.Context, code int, token string, expire time.Time) {
	if keyID := c.GetString(jwtClaimKeyID); keyID != "" {
		_ = singleton.DB.Model(&model.JWTSession{}).
			Where("key_id = ?", keyID).
			Updates(map[string]interface{}{
				"expires_at":   expire,
				"last_used_at": time.Now(),
			}).Error
	}
	setCSRFCookie(c)
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
