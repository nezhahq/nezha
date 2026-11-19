package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func setupAPITokenTest(t *testing.T) func() {
	t.Helper()
	originalDB := singleton.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.APIToken{}, &model.Server{}, &model.MCPOAuthClient{}, &model.MCPOAuthCode{}, &model.MCPOAuthGrant{}))
	singleton.DB = db
	return func() {
		_ = sqlDB.Close()
		singleton.DB = originalDB
	}
}

func ctxAsUser(uid uint64, role model.Role) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", nil)
	c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: uid}, Role: role})
	return c
}

func bindJSON(c *gin.Context, body any) {
	b, _ := json.Marshal(body)
	c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(b))
	c.Request.Header.Set("Content-Type", "application/json")
}

func TestCreateAPIToken_MemberCanCreateExplicitScope(t *testing.T) {
	defer setupAPITokenTest(t)()
	c := ctxAsUser(10, model.RoleMember)
	bindJSON(c, model.APITokenCreateRequest{
		Name:   "claude",
		Scopes: []string{model.ScopeServerRead},
	})
	res, err := createAPIToken(c)
	require.NoError(t, err)
	require.NotEmpty(t, res.Token)
	require.True(t, strings.HasPrefix(res.Token, model.APITokenPrefix))
	require.Greater(t, res.ID, uint64(0))

	var stored model.APIToken
	require.NoError(t, singleton.DB.First(&stored, res.ID).Error)
	require.Equal(t, model.HashAPIToken(res.Token), stored.TokenHash)
	require.Equal(t, uint64(10), stored.UserID)
}

func TestCreateAPIToken_MemberCannotIssueWildcard(t *testing.T) {
	defer setupAPITokenTest(t)()
	c := ctxAsUser(10, model.RoleMember)
	bindJSON(c, model.APITokenCreateRequest{
		Name:   "x",
		Scopes: []string{model.ScopeNezhaAll},
	})
	_, err := createAPIToken(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "admin")
}

func TestCreateAPIToken_AdminCanIssueWildcard(t *testing.T) {
	defer setupAPITokenTest(t)()
	c := ctxAsUser(1, model.RoleAdmin)
	bindJSON(c, model.APITokenCreateRequest{
		Name:   "ops-script",
		Scopes: []string{model.ScopeNezhaAll},
	})
	res, err := createAPIToken(c)
	require.NoError(t, err)
	require.Contains(t, res.Scopes, model.ScopeNezhaAll)
}

func TestCreateAPIToken_RejectsUnknownScope(t *testing.T) {
	defer setupAPITokenTest(t)()
	c := ctxAsUser(1, model.RoleAdmin)
	bindJSON(c, model.APITokenCreateRequest{
		Name:   "x",
		Scopes: []string{"mcp:hack:everything"},
	})
	_, err := createAPIToken(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown scope")
}

func TestCreateAPIToken_RejectsEmptyScopes(t *testing.T) {
	defer setupAPITokenTest(t)()
	c := ctxAsUser(1, model.RoleAdmin)
	bindJSON(c, model.APITokenCreateRequest{Name: "x", Scopes: []string{}})
	_, err := createAPIToken(c)
	require.Error(t, err)
}

func TestCreateAPIToken_RejectsTooManyServerIDs(t *testing.T) {
	defer setupAPITokenTest(t)()
	c := ctxAsUser(1, model.RoleAdmin)
	ids := make([]uint64, 1001)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	bindJSON(c, model.APITokenCreateRequest{
		Name:      "x",
		Scopes:    []string{model.ScopeServerRead},
		ServerIDs: ids,
	})
	_, err := createAPIToken(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "too many server_ids")
}

func TestCreateAPIToken_RejectsExpirationOutOfRange(t *testing.T) {
	defer setupAPITokenTest(t)()
	c := ctxAsUser(1, model.RoleAdmin)
	bindJSON(c, model.APITokenCreateRequest{
		Name:          "x",
		Scopes:        []string{model.ScopeServerRead},
		ExpiresInDays: -1,
	})
	_, err := createAPIToken(c)
	require.Error(t, err)

	bindJSON(c, model.APITokenCreateRequest{
		Name:          "x",
		Scopes:        []string{model.ScopeServerRead},
		ExpiresInDays: 10000,
	})
	_, err = createAPIToken(c)
	require.Error(t, err)
}

func TestDeleteAPIToken_OnlyOwnerOrAdmin(t *testing.T) {
	defer setupAPITokenTest(t)()
	tok := model.APIToken{UserID: 10, Name: "x", TokenHash: model.HashAPIToken("nzp_x")}
	tok.SetScopes([]string{model.ScopeServerRead})
	require.NoError(t, singleton.DB.Create(&tok).Error)

	c := ctxAsUser(11, model.RoleMember)
	c.Params = gin.Params{{Key: "id", Value: itoa(tok.ID)}}
	_, err := deleteAPIToken(c)
	require.Error(t, err, "other member must not delete")

	c = ctxAsUser(10, model.RoleMember)
	c.Params = gin.Params{{Key: "id", Value: itoa(tok.ID)}}
	_, err = deleteAPIToken(c)
	require.NoError(t, err)
}

func TestDeleteAPIToken_AdminCanDeleteAny(t *testing.T) {
	defer setupAPITokenTest(t)()
	tok := model.APIToken{UserID: 10, Name: "x", TokenHash: model.HashAPIToken("nzp_y")}
	tok.SetScopes([]string{model.ScopeServerRead})
	require.NoError(t, singleton.DB.Create(&tok).Error)

	c := ctxAsUser(1, model.RoleAdmin)
	c.Params = gin.Params{{Key: "id", Value: itoa(tok.ID)}}
	_, err := deleteAPIToken(c)
	require.NoError(t, err)
}

// installServerForAPIToken 在 ServerShared 里塞一台属于 ownerUID 的 server，
// 仅用于 PAT 创建路径的 server_ids 权限校验测试。
func installServerForAPIToken(t *testing.T, serverID, ownerUID uint64) func() {
	t.Helper()
	original := singleton.ServerShared
	sc := singleton.NewEmptyServerClassForTest()
	srv := &model.Server{}
	srv.ID = serverID
	srv.SetUserID(ownerUID)
	sc.InsertForTest(srv)
	singleton.ServerShared = sc
	return func() { singleton.ServerShared = original }
}

func TestCreateAPIToken_MemberCannotIncludeForeignServerID(t *testing.T) {
	defer setupAPITokenTest(t)()
	defer installServerForAPIToken(t, 42, 999)() // server 42 owned by user 999

	c := ctxAsUser(10, model.RoleMember) // attacker is user 10
	bindJSON(c, model.APITokenCreateRequest{
		Name:      "evil",
		Scopes:    []string{model.ScopeServerRead},
		ServerIDs: []uint64{42},
	})
	_, err := createAPIToken(c)
	require.Error(t, err, "member must not be able to bind foreign server_id into a PAT")
	require.Contains(t, err.Error(), "permission denied")
}

func TestCreateAPIToken_MemberCannotIncludeNonexistentServerID(t *testing.T) {
	defer setupAPITokenTest(t)()
	cleanup := installServerForAPIToken(t, 1, 10)
	defer cleanup()

	c := ctxAsUser(10, model.RoleMember)
	bindJSON(c, model.APITokenCreateRequest{
		Name:      "evil2",
		Scopes:    []string{model.ScopeServerRead},
		ServerIDs: []uint64{9999}, // never-existed server
	})
	_, err := createAPIToken(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "server not found")
}

func TestCreateAPIToken_AdminCanIncludeAnyServerID(t *testing.T) {
	defer setupAPITokenTest(t)()
	defer installServerForAPIToken(t, 77, 999)() // foreign server

	c := ctxAsUser(1, model.RoleAdmin)
	bindJSON(c, model.APITokenCreateRequest{
		Name:      "ops-script",
		Scopes:    []string{model.ScopeServerRead},
		ServerIDs: []uint64{77},
	})
	res, err := createAPIToken(c)
	require.NoError(t, err)
	require.Equal(t, []uint64{77}, res.ServerIDs)
}

func TestCreateAPIToken_AdminCannotIncludeNonexistentServerID(t *testing.T) {
	defer setupAPITokenTest(t)()
	defer installServerForAPIToken(t, 77, 999)() // only server 77 exists

	c := ctxAsUser(1, model.RoleAdmin)
	bindJSON(c, model.APITokenCreateRequest{
		Name:      "ops-future-bind",
		Scopes:    []string{model.ScopeServerRead},
		ServerIDs: []uint64{4242}, // never-existed server id
	})
	_, err := createAPIToken(c)
	require.Error(t, err, "admin must not bind a PAT to a nonexistent server_id; a later-created server with that id would auto-inherit the grant")
	require.Contains(t, err.Error(), "server not found")
}

func TestCreateAPIToken_MemberOwnServerIDIsAccepted(t *testing.T) {
	defer setupAPITokenTest(t)()
	defer installServerForAPIToken(t, 55, 10)() // user 10 owns server 55

	c := ctxAsUser(10, model.RoleMember)
	bindJSON(c, model.APITokenCreateRequest{
		Name:      "self",
		Scopes:    []string{model.ScopeServerRead},
		ServerIDs: []uint64{55},
	})
	res, err := createAPIToken(c)
	require.NoError(t, err)
	require.Equal(t, []uint64{55}, res.ServerIDs)
}

func TestAPITokenAuthMW_ExpiredTokenRejected(t *testing.T) {
	defer setupAPITokenTest(t)()

	plain := "nzp_" + strings.Repeat("e", 32)
	past := time.Now().Add(-time.Hour)
	tok := model.APIToken{
		UserID:    10,
		Name:      "expired",
		TokenHash: model.HashAPIToken(plain),
		ExpiresAt: &past,
	}
	tok.SetScopes([]string{model.ScopeServerRead})
	require.NoError(t, singleton.DB.Create(&tok).Error)
	require.NoError(t, singleton.DB.Create(&model.User{Common: model.Common{ID: 10}}).Error)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer "+plain)
	apiTokenAuthMiddleware()(c)
	require.True(t, c.IsAborted(), "expired token must abort the request")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "expired")
}

func TestAPITokenAuthMW_OwnerDeletedRejected(t *testing.T) {
	defer setupAPITokenTest(t)()

	plain := "nzp_" + strings.Repeat("o", 32)
	tok := model.APIToken{
		UserID:    999,
		Name:      "orphan",
		TokenHash: model.HashAPIToken(plain),
	}
	tok.SetScopes([]string{model.ScopeServerRead})
	require.NoError(t, singleton.DB.Create(&tok).Error)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer "+plain)
	apiTokenAuthMiddleware()(c)
	require.True(t, c.IsAborted(), "owner-less PAT must abort")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "owner")
}

func TestAPITokenAuthMW_HappyPathSetsUserContext(t *testing.T) {
	defer setupAPITokenTest(t)()

	plain := "nzp_" + strings.Repeat("g", 32)
	tok := model.APIToken{
		UserID:    10,
		Name:      "good",
		TokenHash: model.HashAPIToken(plain),
	}
	tok.SetScopes([]string{model.ScopeServerRead})
	require.NoError(t, singleton.DB.Create(&tok).Error)
	require.NoError(t, singleton.DB.Create(&model.User{Common: model.Common{ID: 10}, Username: "alice"}).Error)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer "+plain)
	apiTokenAuthMiddleware()(c)
	require.False(t, c.IsAborted())

	user, ok := c.Get(model.CtxKeyAuthorizedUser)
	require.True(t, ok)
	require.Equal(t, uint64(10), user.(*model.User).ID)

	got := APITokenFromContext(c)
	require.NotNil(t, got)
	require.Equal(t, "good", got.Name)
}

func TestAPITokenAuthMW_NonNZPBearerPassesThrough(t *testing.T) {
	defer setupAPITokenTest(t)()

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer some-jwt-here")
	apiTokenAuthMiddleware()(c)
	require.False(t, c.IsAborted(), "non-nzp Bearer must pass through to JWT middleware")
	require.Nil(t, APITokenFromContext(c))
}

func TestAPITokenAuthMW_EmptyNZPBodyRejected(t *testing.T) {
	defer setupAPITokenTest(t)()

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer nzp_")
	apiTokenAuthMiddleware()(c)
	require.True(t, c.IsAborted(),
		"Bearer with empty nzp_ body must be rejected (would otherwise hash an empty string and look it up)")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAPITokenAuthMW_RevokedTokenRejected(t *testing.T) {
	defer setupAPITokenTest(t)()

	plain := "nzp_" + strings.Repeat("r", 32)
	tok := model.APIToken{
		UserID:    10,
		Name:      "to-revoke",
		TokenHash: model.HashAPIToken(plain),
	}
	tok.SetScopes([]string{model.ScopeServerRead})
	require.NoError(t, singleton.DB.Create(&tok).Error)
	require.NoError(t, singleton.DB.Create(&model.User{Common: model.Common{ID: 10}}).Error)

	require.NoError(t, singleton.DB.Delete(&model.APIToken{}, tok.ID).Error)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer "+plain)
	apiTokenAuthMiddleware()(c)
	require.True(t, c.IsAborted(), "revoked PAT must abort")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "invalid api token")
}

func TestAPITokenAuthMW_OversizedTokenIsRejected(t *testing.T) {
	defer setupAPITokenTest(t)()

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer nzp_"+strings.Repeat("X", 100*1024))
	apiTokenAuthMiddleware()(c)
	require.True(t, c.IsAborted(), "huge nzp_ body must still abort (no DoS via lookup)")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAPITokenAuthMW_NonASCIITokenIsRejected(t *testing.T) {
	defer setupAPITokenTest(t)()

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer nzp_中文😀💀")
	apiTokenAuthMiddleware()(c)
	require.True(t, c.IsAborted(), "non-ASCII nzp_ body must be rejected by hash lookup")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAPITokenAuthMW_SQLInjectionAttemptIsRejected(t *testing.T) {
	defer setupAPITokenTest(t)()

	plain := "nzp_" + strings.Repeat("s", 32)
	tok := model.APIToken{
		UserID:    10,
		Name:      "real",
		TokenHash: model.HashAPIToken(plain),
	}
	tok.SetScopes([]string{model.ScopeServerRead})
	require.NoError(t, singleton.DB.Create(&tok).Error)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer nzp_'; DROP TABLE api_tokens; --")
	apiTokenAuthMiddleware()(c)
	require.True(t, c.IsAborted(), "SQL-injection-shaped token must just look up and miss")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	var stored model.APIToken
	require.NoError(t, singleton.DB.First(&stored, tok.ID).Error,
		"real token row must survive — GORM uses prepared statements")
}

func TestAPITokenAuthMW_LowercaseBearerPassesThrough(t *testing.T) {
	defer setupAPITokenTest(t)()

	plain := "nzp_" + strings.Repeat("l", 32)
	tok := model.APIToken{UserID: 10, Name: "x", TokenHash: model.HashAPIToken(plain)}
	tok.SetScopes([]string{model.ScopeServerRead})
	require.NoError(t, singleton.DB.Create(&tok).Error)
	require.NoError(t, singleton.DB.Create(&model.User{Common: model.Common{ID: 10}}).Error)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "bearer "+plain)
	apiTokenAuthMiddleware()(c)
	require.False(t, c.IsAborted(),
		"lowercase 'bearer ' is not the canonical scheme; PAT mw must skip it (RFC 7235 says scheme is case-insensitive, "+
			"but we deliberately match GitHub/AWS behaviour of strict 'Bearer ' to keep PAT/JWT lookup paths predictable)")
	require.Nil(t, APITokenFromContext(c), "lowercase bearer must not register as PAT")
}

func TestAPITokenAuthMW_TrailingWhitespaceTolerated(t *testing.T) {
	defer setupAPITokenTest(t)()

	plain := "nzp_" + strings.Repeat("w", 32)
	tok := model.APIToken{UserID: 10, Name: "trim", TokenHash: model.HashAPIToken(plain)}
	tok.SetScopes([]string{model.ScopeServerRead})
	require.NoError(t, singleton.DB.Create(&tok).Error)
	require.NoError(t, singleton.DB.Create(&model.User{Common: model.Common{ID: 10}}).Error)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer  "+plain+"  ")
	apiTokenAuthMiddleware()(c)
	require.False(t, c.IsAborted(),
		"trailing/leading whitespace around PAT must be trimmed (curl users often paste with newlines)")
	require.NotNil(t, APITokenFromContext(c))
}

func TestCreateAPIToken_NameTooLongRejected(t *testing.T) {
	defer setupAPITokenTest(t)()
	c := ctxAsUser(10, model.RoleMember)
	bindJSON(c, model.APITokenCreateRequest{
		Name:   strings.Repeat("X", 129),
		Scopes: []string{model.ScopeServerRead},
	})
	_, err := createAPIToken(c)
	require.Error(t, err, "name >128 chars must be rejected (binding tag max=128 or handler check)")
}

func TestCreateAPIToken_EmptyNameRejected(t *testing.T) {
	defer setupAPITokenTest(t)()
	c := ctxAsUser(10, model.RoleMember)
	bindJSON(c, model.APITokenCreateRequest{
		Name:   "   ",
		Scopes: []string{model.ScopeServerRead},
	})
	_, err := createAPIToken(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "name required")
}

func TestAPIToken_DuplicateHashViolatesUniqueIndex(t *testing.T) {
	defer setupAPITokenTest(t)()

	plain := "nzp_" + strings.Repeat("a", 32)
	for _, uid := range []uint64{10, 11} {
		tok := model.APIToken{
			UserID:    uid,
			Name:      "dup",
			TokenHash: model.HashAPIToken(plain),
		}
		tok.SetScopes([]string{model.ScopeServerRead})
		err := singleton.DB.Create(&tok).Error
		if uid == 10 {
			require.NoError(t, err, "first insert must succeed")
			continue
		}
		require.Error(t, err, "duplicate hash must violate unique index (defense against forged tokens)")
		require.Contains(t, err.Error(), "UNIQUE")
	}
}

func TestListAPITokens_ReturnsOnlyOwn(t *testing.T) {
	defer setupAPITokenTest(t)()
	for i, uid := range []uint64{10, 10, 20} {
		tok := model.APIToken{UserID: uid, Name: "n", TokenHash: model.HashAPIToken("nzp_unique_" + itoa(uint64(i)))}
		tok.SetScopes([]string{model.ScopeServerRead})
		require.NoError(t, singleton.DB.Create(&tok).Error)
	}
	c := ctxAsUser(10, model.RoleMember)
	got, err := listAPITokens(c)
	require.NoError(t, err)
	require.Len(t, got, 2)
}

func itoa(v uint64) string {
	return strings.TrimSpace(jsonNum(v))
}

func jsonNum(v uint64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// scope_doc.go and HasScope advertise nezha:<resource>:* as a first-class
// scope shape, and rest_scope_test.go pins runtime support for it. The
// create-API-token endpoint must accept those wildcards too — otherwise
// the documented surface is unreachable via the only endpoint that can
// issue PATs.
func TestCreateAPIToken_AcceptsResourceWildcardScopes(t *testing.T) {
	defer setupAPITokenTest(t)()

	cases := []string{
		"nezha:server:*",
		"nezha:service:*",
		"nezha:cron:*",
		"nezha:transfer:*",
	}
	for _, scope := range cases {
		t.Run(scope, func(t *testing.T) {
			c := ctxAsUser(10, model.RoleMember)
			bindJSON(c, model.APITokenCreateRequest{
				Name:   "wildcard-" + scope,
				Scopes: []string{scope},
			})
			res, err := createAPIToken(c)
			require.NoError(t, err, "resource wildcard %q must be issuable", scope)
			require.Contains(t, res.Scopes, scope)
		})
	}
}

// nezha:admin:* is admin-only and already on AdminOnlyScopes; this test
// ensures the new wildcard acceptance does NOT widen admin-only scopes
// to members.
func TestCreateAPIToken_ResourceWildcardStillRejectsAdminOnly(t *testing.T) {
	defer setupAPITokenTest(t)()
	c := ctxAsUser(10, model.RoleMember)
	bindJSON(c, model.APITokenCreateRequest{
		Name:   "member-tries-admin-wildcard",
		Scopes: []string{"nezha:admin:*"},
	})
	_, err := createAPIToken(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "admin")
}

// Unknown resources must still be rejected even with a wildcard verb so
// that nezha:bogus:* does not become a forward-compat blank cheque.
func TestCreateAPIToken_RejectsUnknownResourceWildcard(t *testing.T) {
	defer setupAPITokenTest(t)()
	c := ctxAsUser(1, model.RoleAdmin)
	bindJSON(c, model.APITokenCreateRequest{
		Name:   "bogus",
		Scopes: []string{"nezha:bogus:*"},
	})
	_, err := createAPIToken(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown scope")
}

// A password change bumps User.TokenVersion. Like JWT sessions and MCP OAuth
// grants, an API token issued before the bump must stop authenticating, and
// the streams it opened must be dropped.
func TestAPITokenAuthMW_RejectedAfterOwnerCredentialChange(t *testing.T) {
	defer setupAPITokenTest(t)()
	require.NoError(t, singleton.DB.AutoMigrate(&model.Oauth2Bind{}, &model.JWTSession{}))

	originalUserInfoMap, originalAgentSecretToUserID := singleton.UserInfoMap, singleton.AgentSecretToUserId
	originalRegistry := patConnectionRegistryShared
	singleton.UserInfoMap = make(map[uint64]model.UserInfo)
	singleton.AgentSecretToUserId = make(map[string]uint64)
	patConnectionRegistryShared = newPATConnectionRegistry()
	defer func() {
		singleton.UserInfoMap, singleton.AgentSecretToUserId = originalUserInfoMap, originalAgentSecretToUserID
		patConnectionRegistryShared = originalRegistry
	}()

	hash, err := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	require.NoError(t, err)
	owner := model.User{Common: model.Common{ID: 10}, Username: "alice", Password: string(hash), Role: model.RoleMember, TokenVersion: 3}
	require.NoError(t, singleton.DB.Create(&owner).Error)

	issue := ctxAsUser(owner.ID, owner.Role)
	issue.Set(model.CtxKeyAuthorizedUser, &owner)
	bindJSON(issue, model.APITokenCreateRequest{Name: "ci", Scopes: []string{model.ScopeServerExec}})
	issued, err := createAPIToken(issue)
	require.NoError(t, err)

	authenticate := func() (*gin.Context, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/", nil)
		c.Request.Header.Set("Authorization", "Bearer "+issued.Token)
		apiTokenAuthMiddleware()(c)
		return c, w
	}

	c, _ := authenticate()
	require.False(t, c.IsAborted(), "token must work while the owner's credentials are unchanged")
	streamDropped := false
	defer registerPATConnection(c, func() { streamDropped = true })()

	change := ctxAsUser(owner.ID, owner.Role)
	change.Set(model.CtxKeyAuthorizedUser, &owner)
	bindJSON(change, model.ProfileForm{OriginalPassword: "old-password", NewPassword: "new-password"})
	_, err = updateProfile(change)
	require.NoError(t, err)

	c, w := authenticate()
	require.True(t, c.IsAborted(), "token issued before the password change must be rejected")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Nil(t, APITokenFromContext(c))
	require.True(t, streamDropped, "streams opened with the token must be dropped")

	// A token issued after the change carries the new version and works.
	var current model.User
	require.NoError(t, singleton.DB.First(&current, owner.ID).Error)
	issue = ctxAsUser(current.ID, current.Role)
	issue.Set(model.CtxKeyAuthorizedUser, &current)
	bindJSON(issue, model.APITokenCreateRequest{Name: "ci2", Scopes: []string{model.ScopeServerExec}})
	issued, err = createAPIToken(issue)
	require.NoError(t, err)
	c, _ = authenticate()
	require.False(t, c.IsAborted())
}
