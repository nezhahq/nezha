package controller

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

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

func getRedirectURL(c *gin.Context) string {
	scheme := "http://"
	referer := c.Request.Referer()
	if forwardedProto := c.Request.Header.Get("X-Forwarded-Proto"); forwardedProto == "https" || strings.HasPrefix(referer, "https://") {
		scheme = "https://"
	}
	return scheme + c.Request.Host + "/api/v1/oauth2/callback"
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

	rTypeInt, err := strconv.ParseUint(c.Query("type"), 10, 8)
	if err != nil {
		return nil, err
	}

	o2confRaw, has := singleton.Conf.Oauth2[provider]
	if !has {
		return nil, singleton.Localizer.ErrorT("provider not found")
	}
	redirectURL := getRedirectURL(c)
	o2conf := o2confRaw.Setup(redirectURL)

	randomString, err := utils.GenerateRandomString(32)
	if err != nil {
		return nil, err
	}
	state, stateKey := randomString[:16], randomString[16:]
	singleton.Cache.Set(fmt.Sprintf("%s%s", model.CacheKeyOauth2State, stateKey), &model.Oauth2State{
		Action:      model.Oauth2LoginType(rTypeInt),
		Provider:    provider,
		State:       state,
		RedirectURL: redirectURL,
	}, cache.DefaultExpiration)

	url := o2conf.AuthCodeURL(state, oauth2.AccessTypeOnline)
	c.SetCookie("nz-o2s", stateKey, 60*5, "", "", false, false)

	return &model.Oauth2LoginResponse{Redirect: url}, nil
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
	query := singleton.DB.Where("provider = ? AND user_id = ?", provider, u.ID)

	var bindCount int64
	if err := query.Model(&model.Oauth2Bind{}).Count(&bindCount).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	if bindCount < 2 && u.RejectPassword {
		return nil, singleton.Localizer.ErrorT("operation not permitted")
	}

	if err := query.Delete(&model.Oauth2Bind{}).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	return nil, nil
}

// @Summary Oauth2 Callback
// @Description Oauth2 Callback
// @Accept json
// @Produce json
// @Param state query string true "state"
// @Param code query string true "code"
// @Success 200 {object} model.CommonResponse[any]
// @Router /api/v1/oauth2/callback [get]
func oauth2callback(jwtConfig *jwt.GinJWTMiddleware) func(c *gin.Context) (any, error) {
	return func(c *gin.Context) (any, error) {
		callbackData := &model.Oauth2Callback{
			State: c.Query("state"),
			Code:  c.Query("code"),
		}

		state, err := verifyState(c, callbackData.State)
		if err != nil {
			return nil, err
		}

		o2confRaw, has := singleton.Conf.Oauth2[state.Provider]
		if !has {
			return nil, singleton.Localizer.ErrorT("provider not found")
		}

		realip := c.GetString(model.CtxKeyRealIPStr)
		if callbackData.Code == "" {
			model.BlockIP(singleton.DB, realip, model.WAFBlockReasonTypeBruteForceOauth2, model.BlockIDToken)
			return nil, singleton.Localizer.ErrorT("code is required")
		}

		openId, err := exchangeOpenId(c, o2confRaw, callbackData, state.RedirectURL)
		if err != nil {
			model.BlockIP(singleton.DB, realip, model.WAFBlockReasonTypeBruteForceOauth2, model.BlockIDToken)
			return nil, err
		}

		var bind model.Oauth2Bind
		state.Provider = strings.ToLower(state.Provider)
		switch state.Action {
		case model.RTypeBind:
			u, authorized := c.Get(model.CtxKeyAuthorizedUser)
			if !authorized {
				return nil, singleton.Localizer.ErrorT("unauthorized")
			}
			user := u.(*model.User)

			result := singleton.DB.Where("provider = ? AND open_id = ?", state.Provider, openId).Limit(1).Find(&bind)
			if result.Error != nil && result.Error != gorm.ErrRecordNotFound {
				return nil, newGormError("%v", result.Error)
			}
			bind.UserID = user.ID
			bind.Provider = state.Provider
			bind.OpenID = openId

			if result.Error == gorm.ErrRecordNotFound {
				result = singleton.DB.Create(&bind)
			} else {
				result = singleton.DB.Save(&bind)
			}
			if result.Error != nil {
				return nil, newGormError("%v", result.Error)
			}
		default:
			if err := singleton.DB.Where("provider = ? AND open_id = ?", state.Provider, openId).First(&bind).Error; err != nil {
				return nil, singleton.Localizer.ErrorT("oauth2 user not binded yet")
			}
		}

		tokenString, _, err := jwtConfig.TokenGenerator(map[string]interface{}{
			"user_id": fmt.Sprintf("%d", bind.UserID),
			"ip":      realip,
		})
		if err != nil {
			return nil, err
		}

		jwtConfig.SetCookie(c, tokenString)
		c.Redirect(http.StatusFound, utils.IfOr(state.Action == model.RTypeBind, "/dashboard/profile?oauth2=true", "/dashboard/login?oauth2=true"))

		return nil, errNoop
	}
}

func exchangeOpenId(c *gin.Context, o2confRaw *model.Oauth2Config,
	callbackData *model.Oauth2Callback, redirectURL string) (string, error) {
	o2conf := o2confRaw.Setup(redirectURL)

	otk, err := o2conf.Exchange(c, callbackData.Code)
	if err != nil {
		return "", err
	}
	oauth2client := o2conf.Client(c, otk)
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

func verifyState(c *gin.Context, state string) (*model.Oauth2State, error) {
	// 验证登录跳转时的 State
	stateKey, err := c.Cookie("nz-o2s")
	if err != nil {
		return nil, singleton.Localizer.ErrorT("invalid state key")
	}

	cacheKey := fmt.Sprintf("%s%s", model.CacheKeyOauth2State, stateKey)
	istate, ok := singleton.Cache.Get(cacheKey)
	if !ok {
		return nil, singleton.Localizer.ErrorT("invalid state key")
	}

	oauth2State, ok := istate.(*model.Oauth2State)
	if !ok || oauth2State.State != state {
		return nil, singleton.Localizer.ErrorT("invalid state key")
	}

	return oauth2State, nil
}
