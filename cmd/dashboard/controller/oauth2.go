package controller

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"github.com/patrickmn/go-cache"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

type Oauth2LoginType uint8

const (
	_ Oauth2LoginType = iota
	rTypeLogin
	rTypeBind
)

func getRedirectURL(c *gin.Context, provider string, rType Oauth2LoginType) string {
	scheme := "http://"
	referer := c.Request.Referer()
	if forwardedProto := c.Request.Header.Get("X-Forwarded-Proto"); forwardedProto == "https" || strings.HasPrefix(referer, "https://") {
		scheme = "https://"
	}
	var suffix string
	if rType == rTypeLogin {
		suffix = "/dashboard/login?provider=" + provider
	} else if rType == rTypeBind {
		suffix = "/dashboard/profile?provider=" + provider
	}
	return scheme + c.Request.Host + suffix
}

// @Summary Get Oauth2 Redirect URL
// @Description Get Oauth2 Redirect URL
// @Produce json
// @Param provider path string true "provider"
// @Param type query int false "type" Enums(1, 2) default(1)
// @Success 200 {object} model.Oauth2LoginResponse
// @Router /api/v1/oauth2/{provider} [get]
func oauth2redirect(c *gin.Context) (*model.Oauth2LoginResponse, error) {
	provider := c.Param("provider")
	if provider == "" {
		return nil, singleton.Localizer.ErrorT("provider is required")
	}

	rTypeInt, err := strconv.Atoi(c.Query("type"))
	if err != nil {
		return nil, err
	}

	o2confRaw, has := singleton.Conf.Oauth2[provider]
	if !has {
		return nil, singleton.Localizer.ErrorT("provider not found")
	}
	o2conf := o2confRaw.Setup(getRedirectURL(c, provider, Oauth2LoginType(rTypeInt)))

	randomString, err := utils.GenerateRandomString(32)
	if err != nil {
		return nil, err
	}
	state, stateKey := randomString[:16], randomString[16:]
	singleton.Cache.Set(fmt.Sprintf("%s%s", model.CacheKeyOauth2State, stateKey), state, cache.DefaultExpiration)

	url := o2conf.AuthCodeURL(state, oauth2.AccessTypeOnline)
	c.SetCookie("nz-o2s", stateKey, 60*5, "", "", false, false)

	return &model.Oauth2LoginResponse{Redirect: url}, nil
}

func exchangeOpenId(c *gin.Context, o2confRaw *model.Oauth2Config, provider string, callbackData model.Oauth2Callback, typ Oauth2LoginType) (string, error) {
	// 验证登录跳转时的 State
	stateKey, err := c.Cookie("nz-o2s")
	if err != nil {
		return "", singleton.Localizer.ErrorT("invalid state key")
	}
	state, ok := singleton.Cache.Get(fmt.Sprintf("%s%s", model.CacheKeyOauth2State, stateKey))
	if !ok || state.(string) != callbackData.State {
		return "", singleton.Localizer.ErrorT("invalid state key")
	}

	o2conf := o2confRaw.Setup(getRedirectURL(c, provider, typ))

	ctx := context.Background()

	otk, err := o2conf.Exchange(ctx, callbackData.Code)
	if err != nil {
		return "", err
	}
	oauth2client := o2conf.Client(ctx, otk)
	resp, err := oauth2client.Get(o2confRaw.UserInfoURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return gjson.GetBytes(body, o2confRaw.UserIDPath).String(), nil
}

// @Summary Oauth2 Callback
// @Description Oauth2 Callback
// @Accept json
// @Produce json
// @Param provider path string true "provider"
// @Param body body model.Oauth2Callback true "body"
// @Success 200 {object} model.LoginResponse
// @Router /api/v1/oauth2/{provider}/callback [post]
func oauth2callback(jwtConfig *jwt.GinJWTMiddleware) func(c *gin.Context) (*model.LoginResponse, error) {
	return func(c *gin.Context) (*model.LoginResponse, error) {
		provider := c.Param("provider")
		if provider == "" {
			return nil, singleton.Localizer.ErrorT("provider is required")
		}

		o2confRaw, has := singleton.Conf.Oauth2[provider]
		if !has {
			return nil, singleton.Localizer.ErrorT("provider not found")
		}

		var callbackData model.Oauth2Callback
		if err := c.ShouldBind(&callbackData); err != nil {
			return nil, err
		}

		realip := c.GetString(model.CtxKeyRealIPStr)

		if callbackData.Code == "" {
			model.BlockIP(singleton.DB, realip, model.WAFBlockReasonTypeBruteForceOauth2, model.BlockIDToken)
			return nil, singleton.Localizer.ErrorT("code is required")
		}

		openId, err := exchangeOpenId(c, o2confRaw, provider, callbackData, rTypeLogin)
		if err != nil {
			model.BlockIP(singleton.DB, realip, model.WAFBlockReasonTypeBruteForceOauth2, model.BlockIDToken)
			return nil, err
		}

		var bind model.Oauth2Bind
		if err := singleton.DB.Where("provider = ? AND open_id = ?", strings.ToLower(provider), openId).First(&bind).Error; err != nil {
			return nil, singleton.Localizer.ErrorT("oauth2 user not binded yet")
		}

		tokenString, expire, err := jwtConfig.TokenGenerator(fmt.Sprintf("%d", bind.UserID))
		if err != nil {
			return nil, err
		}

		jwtConfig.SetCookie(c, tokenString)

		return &model.LoginResponse{Token: tokenString, Expire: expire.Format(time.RFC3339)}, nil
	}
}

// @Summary Bind Oauth2
// @Description Bind Oauth2
// @Accept json
// @Produce json
// @Param provider path string true "provider"
// @Param body body model.Oauth2Callback true "body"
// @Success 200 {object} any
// @Router /api/v1/oauth2/{provider}/bind [post]
func bindOauth2(c *gin.Context) (any, error) {
	var bindData model.Oauth2Callback
	if err := c.ShouldBind(&bindData); err != nil {
		return nil, err
	}

	provider := c.Param("provider")
	o2conf, has := singleton.Conf.Oauth2[provider]
	if !has {
		return nil, singleton.Localizer.ErrorT("provider not found")
	}

	openId, err := exchangeOpenId(c, o2conf, provider, bindData, rTypeBind)
	if err != nil {
		return nil, err
	}

	u := c.MustGet(model.CtxKeyAuthorizedUser).(*model.User)
	provider = strings.ToLower(provider)

	var bind model.Oauth2Bind
	result := singleton.DB.Where("provider = ? AND open_id = ?", provider, openId).Limit(1).Find(&bind)
	if result.Error != nil && result.Error != gorm.ErrRecordNotFound {
		return nil, newGormError("%v", result.Error)
	}
	bind.UserID = u.ID
	bind.Provider = provider
	bind.OpenID = openId
	if result.Error == gorm.ErrRecordNotFound {
		result = singleton.DB.Create(&bind)
	} else {
		result = singleton.DB.Save(&bind)
	}
	if result.Error != nil {
		return nil, newGormError("%v", result.Error)
	}
	return nil, nil
}

// @Summary Unbind Oauth2
// @Description Unbind Oauth2
// @Accept json
// @Produce json
// @Param provider path string true "provider"
// @Success 200 {object} any
// @Router /api/v1/oauth2/{provider}/unbind [post]
func unbindOauth2(c *gin.Context) (any, error) {
	provider := c.Param("provider")
	if provider == "" {
		return nil, singleton.Localizer.ErrorT("provider is required")
	}
	_, has := singleton.Conf.Oauth2[provider]
	if !has {
		return nil, singleton.Localizer.ErrorT("provider not found")
	}
	provider = strings.ToLower(provider)
	u := c.MustGet(model.CtxKeyAuthorizedUser).(*model.User)
	if err := singleton.DB.Where("provider = ? AND user_id = ?", provider, u.ID).Delete(&model.Oauth2Bind{}).Error; err != nil {
		return nil, newGormError("%v", err)
	}
	return nil, nil
}
