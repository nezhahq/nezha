package controller

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

const (
	apiTokenSecretLength     = 32                         // 明文 token 随机部分长度（hex 编码前）
	apiTokenCtxKey           = "nz_api_token"             // #nosec G101 -- gin context key name, not a credential
	apiTokenLastUsedCtxKey   = "nz_api_token_used_marker" // #nosec G101 -- gin context key name, not a credential
	apiTokenReadOnlyCtxKey   = "nz_api_token_read_only"   // #nosec G101 -- gin context key name, not a credential
	apiTokenAuthSchemePrefix = "Bearer "
)

// listAPITokens 列出当前用户的所有 PAT（脱敏，不含 token 明文）。
// @Summary List API tokens
// @Tags auth required
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.APITokenView]
// @Router /api-tokens [get]
func listAPITokens(c *gin.Context) ([]model.APITokenView, error) {
	uid := getUid(c)
	var rows []model.APIToken
	if err := singleton.DB.Where("user_id = ?", uid).Order("id DESC").Find(&rows).Error; err != nil {
		return nil, newGormError("%v", err)
	}
	out := make([]model.APITokenView, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ToView())
	}
	return out, nil
}

// createAPIToken 创建一个 PAT。明文 token 仅在响应中返回一次。
// @Summary Create API token
// @Tags auth required
// @Accept json
// @Param body body model.APITokenCreateRequest true "request"
// @Produce json
// @Success 200 {object} model.CommonResponse[model.APITokenCreateResponse]
// @Router /api-tokens [post]
func createAPIToken(c *gin.Context) (*model.APITokenCreateResponse, error) {
	var req model.APITokenCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return nil, err
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return nil, errors.New("name required")
	}
	if len(req.Name) > 128 {
		return nil, errors.New("name too long (max 128 chars)")
	}
	if req.ExpiresInDays < 0 {
		return nil, errors.New("expires_in_days must be >= 0")
	}
	if req.ExpiresInDays > 3650 {
		return nil, errors.New("expires_in_days too large (max 3650, i.e. 10 years)")
	}
	if len(req.Scopes) > 32 {
		return nil, errors.New("too many scopes (max 32)")
	}
	if len(req.ServerIDs) > 1000 {
		return nil, errors.New("too many server_ids (max 1000)")
	}

	allowed := append(append([]string{}, model.AllScopes...), model.AdminOnlyScopes...)
	seen := make(map[string]struct{}, len(req.Scopes))
	cleaned := make([]string, 0, len(req.Scopes))
	for _, s := range req.Scopes {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		normalized, ok := model.NormalizeIncomingScope(s)
		if !ok {
			return nil, errors.New("unknown scope: " + s)
		}
		if !slices.Contains(allowed, normalized) {
			return nil, errors.New("unknown scope: " + s)
		}
		if _, dup := seen[normalized]; dup {
			continue
		}
		seen[normalized] = struct{}{}
		cleaned = append(cleaned, normalized)
	}
	if len(cleaned) == 0 {
		return nil, errors.New("at least one scope required")
	}

	if !callerIsAdmin(c) {
		for _, s := range cleaned {
			if slices.Contains(model.AdminOnlyScopes, s) {
				return nil, errors.New("only admin can issue scope: " + s)
			}
		}
	}

	if len(req.ServerIDs) > 0 {
		seenSrv := make(map[uint64]struct{}, len(req.ServerIDs))
		deduped := make([]uint64, 0, len(req.ServerIDs))
		for _, sid := range req.ServerIDs {
			if sid == 0 {
				return nil, errors.New("server_id 0 is invalid")
			}
			if _, dup := seenSrv[sid]; dup {
				continue
			}
			seenSrv[sid] = struct{}{}
			deduped = append(deduped, sid)
			server, _ := singleton.ServerShared.Get(sid)
			if server == nil {
				return nil, errors.New("server not found")
			}
			if !callerIsAdmin(c) && !server.HasPermission(c) {
				return nil, errors.New("permission denied on server")
			}
		}
		req.ServerIDs = deduped
	}

	secret, err := utils.GenerateRandomString(apiTokenSecretLength)
	if err != nil {
		return nil, err
	}
	plaintext := model.APITokenPrefix + secret

	tok := model.APIToken{
		UserID:    getUid(c),
		Name:      req.Name,
		TokenHash: model.HashAPIToken(plaintext),
	}
	tok.SetScopes(cleaned)
	if len(req.ServerIDs) > 0 {
		tok.SetServerIDs(req.ServerIDs)
	}
	if req.ExpiresInDays > 0 {
		exp := time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		tok.ExpiresAt = &exp
	}

	if err := singleton.DB.Create(&tok).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	return &model.APITokenCreateResponse{
		ID:        tok.ID,
		Name:      tok.Name,
		Token:     plaintext,
		Scopes:    tok.Scopes(),
		ServerIDs: tok.ServerIDs(),
		ExpiresAt: tok.ExpiresAt,
	}, nil
}

// deleteAPIToken 吊销一个 PAT。
// @Summary Revoke API token
// @Tags auth required
// @Param id path uint true "token id"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /api-tokens/{id} [delete]
func deleteAPIToken(c *gin.Context) (any, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}
	q := singleton.DB.Where("id = ?", id)
	if !callerIsAdmin(c) {
		q = q.Where("user_id = ?", getUid(c))
	}
	res := q.Delete(&model.APIToken{})
	if res.Error != nil {
		return nil, newGormError("%v", res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, errors.New("not found")
	}
	// Fan out the revocation to any active long-lived connection that
	// carries this PAT — ws/server, ws/transfer, terminal, FM. Without
	// this hook a deleted PAT keeps streaming until the underlying
	// connection naturally drops.
	patConnectionRegistryShared.revokeToken(id)
	return nil, nil
}

// apiTokenAuthMiddleware 解析 `Authorization: Bearer nzp_xxx`，
// 命中后把 *model.User 挂到 ctx 上，使下游一切 Server.HasPermission/getUid 复用 JWT 路径。
//
// 不命中（无 Authorization 头或前缀不是 nzp_）：放行下一个中间件（例如 JWT）。
// 命中但 token 无效：直接 401 并 abort，不再走到 JWT。
func apiTokenAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		readOnly := apiTokenAuthReadOnly(c)
		raw := strings.TrimSpace(c.GetHeader("Authorization"))
		if raw == "" {
			return
		}
		if !strings.HasPrefix(raw, apiTokenAuthSchemePrefix) {
			return
		}
		plaintext := strings.TrimSpace(strings.TrimPrefix(raw, apiTokenAuthSchemePrefix))
		if !strings.HasPrefix(plaintext, model.APITokenPrefix) {
			// 既然有 Bearer 但不是 PAT 前缀，交给后续 JWT 中间件处理
			return
		}

		realIP := c.GetString(model.CtxKeyRealIPStr)

		var tok model.APIToken
		err := singleton.DB.Where("token_hash = ?", model.HashAPIToken(plaintext)).First(&tok).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if !readOnly {
					model.BlockIP(singleton.DB, realIP, model.WAFBlockReasonTypeBruteForceToken, model.BlockIDToken)
				}
				abortAPITokenUnauthorized(c, "invalid api token")
				return
			}
			abortAPITokenUnauthorized(c, "api token lookup failed")
			return
		}
		authenticateAPITokenIdentity(c, &tok)
	}
}

// Share the existing ownership, expiry, audit identity and last-used handling
// with MCP-only OAuth credentials without accepting them on REST routes.
func authenticateAPITokenIdentity(c *gin.Context, tok *model.APIToken) {
	readOnly := apiTokenAuthReadOnly(c)
	realIP := c.GetString(model.CtxKeyRealIPStr)
	now := time.Now()
	if tok.IsExpired(now) {
		if !readOnly {
			model.BlockIP(singleton.DB, realIP, model.WAFBlockReasonTypeBruteForceToken, model.BlockIDToken)
		}
		abortAPITokenUnauthorized(c, "api token expired")
		return
	}

	var user model.User
	if err := singleton.DB.First(&user, tok.UserID).Error; err != nil {
		if !readOnly {
			model.BlockIP(singleton.DB, realIP, model.WAFBlockReasonTypeBruteForceToken, model.BlockIDToken)
		}
		abortAPITokenUnauthorized(c, "owner of api token not found")
		return
	}

	if !readOnly {
		model.UnblockIP(singleton.DB, realIP, model.BlockIDToken)
	}

	c.Set(model.CtxKeyAuthorizedUser, &user)
	c.Set(apiTokenCtxKey, tok)
	c.Set(model.CtxKeyAPIToken, tok)

	// last_used 同步更新：开销极低（一行 UPDATE），异步路径在
	// 多连接 sqlite 测试场景下会和测试 teardown 形成竞态，并把
	// `last_used_*` 写丢到不可见的 :memory: 实例。生产路径上等价。
	if !readOnly && apiTokenLastUsedOnce(c) {
		c.Set(apiTokenLastUsedCtxKey, true)
		_ = singleton.DB.Model(&model.APIToken{}).
			Where("id = ?", tok.ID).
			Updates(map[string]any{
				"last_used_at": now,
				"last_used_ip": c.GetString(model.CtxKeyRealIPStr),
			}).Error
	}
}

func abortAPITokenUnauthorized(c *gin.Context, reason string) {
	if c.FullPath() == "/mcp" {
		mcpOAuthSetChallenge(c)
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, model.CommonResponse[any]{
		Success: false,
		Error:   "ApiErrorUnauthorized: " + reason,
	})
}

// APITokenFromContext 取当前请求关联的 PAT，未命中返回 nil。
// MCP tool 中间件用它做 scope 校验（闸 2）。
func APITokenFromContext(c *gin.Context) *model.APIToken {
	v, ok := c.Get(apiTokenCtxKey)
	if !ok {
		return nil
	}
	t, _ := v.(*model.APIToken)
	return t
}
