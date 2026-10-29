package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/frontend"
	"github.com/nezhahq/nezha/pkg/i18n"
	"github.com/nezhahq/nezha/service/singleton"
)

func frontendManagementFixture(t *testing.T) string {
	t.Helper()
	previousConf, previousLocalizer := singleton.Conf, singleton.Localizer
	previousTemplates := singleton.FrontendTemplates
	t.Cleanup(func() {
		singleton.Conf, singleton.Localizer = previousConf, previousLocalizer
		singleton.FrontendTemplates = previousTemplates
	})
	require.NoError(t, singleton.InitFrontendTemplates())
	singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	root := t.TempDir()
	for _, template := range []frontend.Template{
		{Path: "installed-user", Name: "User release", Custom: true},
		{Path: "installed-admin", Name: "Admin release", Custom: true, IsAdmin: true},
	} {
		directory := filepath.Join(root, "frontends", template.Path)
		require.NoError(t, os.MkdirAll(filepath.Join(directory, "dist"), 0750))
		metadata, err := json.Marshal(template)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(directory, "metadata.json"), metadata, 0600))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "dist", "index.html"), []byte(template.Name), 0600))
	}
	configPath := filepath.Join(root, "config.yaml")
	require.NoError(t, singleton.InitConfigFromPath(configPath))
	return configPath
}

func frontendManagementRouter(user *model.User, token *model.APIToken, middleware ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	auth := router.Group("/api/v1", func(c *gin.Context) {
		if user != nil {
			c.Set(model.CtxKeyAuthorizedUser, user)
		}
		if token != nil {
			c.Set(model.CtxKeyAPIToken, token)
			c.Set(apiTokenCtxKey, token)
		}
	})
	auth.Use(middleware...)
	registerFrontendRoutes(auth)
	router.NoRoute(fallbackToFrontend(fstest.MapFS{
		"user-dist/index.html":  &fstest.MapFile{Data: []byte("Built-in user")},
		"admin-dist/index.html": &fstest.MapFile{Data: []byte("Built-in admin")},
	}))
	return router
}

func frontendManagementRequest(t *testing.T, router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestFrontendManagementRejectsUnauthorizedRolesAndScopes(t *testing.T) {
	frontendManagementFixture(t)
	for _, actor := range []struct {
		name  string
		user  *model.User
		token *model.APIToken
	}{
		{name: "anonymous"},
		{name: "member", user: &model.User{Role: model.RoleMember}},
		{name: "member with admin scope", user: &model.User{Role: model.RoleMember}, token: &model.APIToken{ScopesCSV: model.ScopeAdminAll}},
		{name: "administrator without scope", user: &model.User{Role: model.RoleAdmin}, token: &model.APIToken{ScopesCSV: model.ScopeServerRead}},
	} {
		t.Run(actor.name, func(t *testing.T) {
			router := frontendManagementRouter(actor.user, actor.token)
			for _, endpoint := range []struct{ method, path, body string }{
				{"GET", "/api/v1/frontend", ""},
				{"GET", "/api/v1/frontend/releases?repository=https://github.com/owner/site", ""},
				{"POST", "/api/v1/frontend/install", `{"repository":"https://github.com/owner/site","tag":"v1"}`},
				{"POST", "/api/v1/frontend/activate", `{"path":"installed-user"}`},
				{"POST", "/api/v1/frontend/remove", `{"path":"installed-user"}`},
			} {
				response := frontendManagementRequest(t, router, endpoint.method, endpoint.path, endpoint.body)
				var result model.CommonResponse[any]
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
				require.False(t, result.Success, "%s: %s", endpoint.path, response.Body.String())
				require.NotEmpty(t, result.Error)
				if actor.token != nil && actor.user.Role == model.RoleAdmin {
					require.Equal(t, http.StatusForbidden, response.Code)
				}
			}
			user, admin := singleton.Conf.FrontendSelection()
			require.Equal(t, "user-dist", user)
			require.Equal(t, "admin-dist", admin)
		})
	}
}

func TestFrontendActivationChangesContentPersistsAndRestoresBuiltins(t *testing.T) {
	configPath := frontendManagementFixture(t)
	router := frontendManagementRouter(&model.User{Role: model.RoleAdmin}, &model.APIToken{ScopesCSV: model.ScopeAdminAll})
	status := frontendManagementRequest(t, router, "GET", "/api/v1/frontend", "")
	var catalog model.CommonResponse[frontendStatus]
	require.NoError(t, json.Unmarshal(status.Body.Bytes(), &catalog))
	require.True(t, catalog.Success)
	require.GreaterOrEqual(t, len(catalog.Data.Templates), 4)
	for _, selection := range []struct {
		path          string
		admin         bool
		page, content string
	}{
		{"installed-user", false, "/", "User release"},
		{"installed-admin", true, "/dashboard/settings/frontends", "Admin release"},
		{"user-dist", false, "/", "Built-in user"},
		{"admin-dist", true, "/dashboard/settings/frontends", "Built-in admin"},
	} {
		body, err := json.Marshal(frontendActivateForm{Path: selection.path, IsAdmin: selection.admin})
		require.NoError(t, err)
		response := frontendManagementRequest(t, router, "POST", "/api/v1/frontend/activate", string(body))
		var result model.CommonResponse[any]
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.True(t, result.Success, response.Body.String())
		user, admin := singleton.Conf.FrontendSelection()
		require.NoError(t, singleton.InitConfigFromPath(configPath))
		reloadedUser, reloadedAdmin := singleton.Conf.FrontendSelection()
		require.Equal(t, user, reloadedUser)
		require.Equal(t, admin, reloadedAdmin)
		request := httptest.NewRequest("GET", selection.page, nil)
		request.Header.Set("If-Modified-Since", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
		page := httptest.NewRecorder()
		router.ServeHTTP(page, request)
		require.Equal(t, http.StatusOK, page.Code)
		require.Equal(t, selection.content, page.Body.String())
		require.Equal(t, "no-store", page.Header().Get("Cache-Control"))
		if selection.path == "installed-user" {
			require.Equal(t, "Built-in admin", frontendManagementRequest(t, router, "GET", "/dashboard/", "").Body.String())
		}
	}
}

func TestFrontendActivationRejectsWrongRoleAndRollsBackSaveFailure(t *testing.T) {
	configPath := frontendManagementFixture(t)
	router := frontendManagementRouter(&model.User{Role: model.RoleAdmin}, nil)
	for _, body := range []string{`{"path":"missing"}`, `{"path":"installed-admin"}`, `{"path":"installed-user","is_admin":true}`, `{"path":"../../etc"}`, `{"path":"/etc"}`, `{"path":"installed-user/../installed-admin"}`} {
		response := frontendManagementRequest(t, router, "POST", "/api/v1/frontend/activate", body)
		var result model.CommonResponse[any]
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.False(t, result.Success)
		require.Contains(t, result.Error, "not installed")
	}
	// A directory at the config path forces a real persistence error, even as root.
	require.NoError(t, os.Remove(configPath))
	require.NoError(t, os.Mkdir(configPath, 0750))
	response := frontendManagementRequest(t, router, "POST", "/api/v1/frontend/activate", `{"path":"installed-user"}`)
	var result model.CommonResponse[any]
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.False(t, result.Success)
	user, admin := singleton.Conf.FrontendSelection()
	require.Equal(t, "user-dist", user)
	require.Equal(t, "admin-dist", admin)
}

func TestInstalledFrontendPreservesStaticAssetHTTPStatus(t *testing.T) {
	frontendManagementFixture(t)
	user, admin := "installed-user", "installed-admin"
	require.NoError(t, singleton.Conf.CommitFrontendSelection(&user, &admin, singleton.Conf.Save))
	router := frontendManagementRouter(nil, nil)
	for _, frontend := range []struct{ template, prefix string }{{user, "/"}, {admin, "/dashboard/"}} {
		for _, asset := range []string{"application.js", "application.css"} {
			t.Run(frontend.template+"/"+asset, func(t *testing.T) {
				const content = "frontend-asset-content"
				require.NoError(t, os.WriteFile(filepath.Join(singleton.Conf.FrontendStore.Directory(frontend.template), asset), []byte(content), 0600))
				url := frontend.prefix + asset
				initial := frontendManagementRequest(t, router, http.MethodGet, url, "")
				require.Equal(t, http.StatusOK, initial.Code)
				require.Equal(t, content, initial.Body.String())
				etag := initial.Header().Get("ETag")
				require.NotEmpty(t, etag)
				require.Empty(t, initial.Header().Get("Last-Modified"))
				for _, conditional := range []struct {
					name, header, value string
					status              int
					body                string
				}{
					{"cached", "If-None-Match", etag, http.StatusNotModified, ""},
					{"partial", "Range", "bytes=0-7", http.StatusPartialContent, content[:8]},
				} {
					t.Run(conditional.name, func(t *testing.T) {
						request := httptest.NewRequest(http.MethodGet, url, nil)
						request.Header.Set(conditional.header, conditional.value)
						response := httptest.NewRecorder()
						router.ServeHTTP(response, request)
						require.Equal(t, conditional.status, response.Code)
						require.Equal(t, conditional.body, response.Body.String())
						if conditional.status == http.StatusPartialContent {
							require.Equal(t, "bytes 0-7/22", response.Header().Get("Content-Range"))
						}
					})
				}
			})
		}
		missing := frontendManagementRequest(t, router, http.MethodGet, frontend.prefix+"missing-page", "")
		require.Equal(t, http.StatusNotFound, missing.Code)
		require.Contains(t, missing.Body.String(), "release")
	}
}

func TestFrontendManagementCSRFRejectsMissingAndForgedTokens(t *testing.T) {
	frontendManagementFixture(t)
	router := frontendManagementRouter(&model.User{Role: model.RoleAdmin}, nil, csrfMiddleware())
	for _, endpoint := range []struct{ path, body string }{
		{"/api/v1/frontend/install", `{"repository":"https://github.com/owner/site","tag":"v1"}`},
		{"/api/v1/frontend/activate", `{"path":"installed-user"}`},
		{"/api/v1/frontend/remove", `{"path":"installed-user"}`},
	} {
		for _, token := range []string{"", "forged-csrf-token"} {
			request := httptest.NewRequest("POST", endpoint.path, strings.NewReader(endpoint.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer nzp_forged")
			request.AddCookie(&http.Cookie{Name: "nz-jwt", Value: "authenticated-session"})
			if token != "" {
				request.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
				request.Header.Set(csrfHeaderName, token)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, http.StatusForbidden, response.Code)
			var result model.CommonResponse[any]
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
			require.False(t, result.Success)
			require.Contains(t, result.Error, "CSRF")
		}
	}
	user, admin := singleton.Conf.FrontendSelection()
	require.Equal(t, "user-dist", user)
	require.Equal(t, "admin-dist", admin)
	// A real signed pair must reach the same activation handler successfully.
	token := issueCSRFToken()
	request := httptest.NewRequest("POST", "/api/v1/frontend/activate", strings.NewReader(`{"path":"installed-user"}`))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
	request.Header.Set(csrfHeaderName, token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var result model.CommonResponse[any]
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success, response.Body.String())
	user, admin = singleton.Conf.FrontendSelection()
	require.Equal(t, "installed-user", user)
	require.Equal(t, "admin-dist", admin)
}

func TestInstalledFrontendBlocksTraversalAndEscapingSymlinks(t *testing.T) {
	configPath := frontendManagementFixture(t)
	secretPath := filepath.Join(filepath.Dir(configPath), "private-canary.txt")
	require.NoError(t, os.WriteFile(secretPath, []byte("private-canary-content"), 0600))
	user, admin := "installed-user", "installed-admin"
	require.NoError(t, singleton.Conf.CommitFrontendSelection(&user, &admin, singleton.Conf.Save))
	for _, selection := range []string{user, admin} {
		require.NoError(t, os.Symlink(secretPath, filepath.Join(singleton.Conf.FrontendStore.Directory(selection), "outside-link.txt")))
	}
	// Static resources are public, so confinement must also hold without a login.
	router := frontendManagementRouter(nil, nil)
	for _, prefix := range []string{"/", "/dashboard/"} {
		for _, path := range []string{
			"../../../private-canary.txt", "%2e%2e/%2e%2e/%2e%2e/private-canary.txt",
			"%2e%2e%2f%2e%2e%2f%2e%2e%2fprivate-canary.txt", "%252e%252e/private-canary.txt",
			"..%5c..%5cprivate-canary.txt", "outside-link.txt", "../metadata.json",
		} {
			response := frontendManagementRequest(t, router, "GET", prefix+path, "")
			require.NotContains(t, response.Body.String(), "private-canary-content", prefix+path)
			require.NotContains(t, response.Body.String(), `"custom":true`, prefix+path)
			require.NotContains(t, response.Body.String(), `"path":`, prefix+path)
		}
		response := frontendManagementRequest(t, router, "GET", prefix+"index.html", "")
		require.Equal(t, http.StatusOK, response.Code)
		if prefix == "/" {
			require.Equal(t, "User release", response.Body.String())
		} else {
			require.Equal(t, "Admin release", response.Body.String())
		}
	}
}

func TestFrontendSwitchInvalidatesSameNameAssetCache(t *testing.T) {
	configPath := frontendManagementFixture(t)
	for _, role := range []struct {
		name, prefix string
		admin        bool
	}{{"user", "/", false}, {"admin", "/dashboard/", true}} {
		t.Run(role.name, func(t *testing.T) {
			currentID, previousID := "installed-"+role.name, "installed-"+role.name+"-previous"
			previousDirectory := filepath.Join(filepath.Dir(configPath), "frontends", previousID)
			require.NoError(t, os.MkdirAll(filepath.Join(previousDirectory, "dist"), 0750))
			metadata, err := json.Marshal(frontend.Template{Path: previousID, Custom: true, IsAdmin: role.admin})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(previousDirectory, "metadata.json"), metadata, 0600))
			require.NoError(t, os.WriteFile(filepath.Join(previousDirectory, "dist", "index.html"), []byte("previous frontend"), 0600))
			require.NoError(t, singleton.InitConfigFromPath(configPath))
			activate := func(id string) {
				var user, admin *string
				if role.admin {
					admin = &id
				} else {
					user = &id
				}
				require.NoError(t, singleton.Conf.CommitFrontendSelection(user, admin, singleton.Conf.Save))
			}
			router := frontendManagementRouter(nil, nil)
			for _, asset := range []string{"application.js", "application.css"} {
				t.Run(asset, func(t *testing.T) {
					for _, version := range []struct {
						id, content string
						modified    time.Time
					}{
						{currentID, "current asset", time.Now().Add(-time.Hour)},
						{previousID, "previous asset", time.Now().Add(-2 * time.Hour)},
					} {
						filename := filepath.Join(singleton.Conf.FrontendStore.Directory(version.id), asset)
						require.NoError(t, os.WriteFile(filename, []byte(version.content), 0600))
						require.NoError(t, os.Chtimes(filename, version.modified, version.modified))
					}
					url := role.prefix + asset
					requestAsset := func(etag string) *httptest.ResponseRecorder {
						request := httptest.NewRequest(http.MethodGet, url, nil)
						request.Header.Set("If-Modified-Since", time.Now().UTC().Format(http.TimeFormat))
						if etag != "" {
							request.Header.Set("If-None-Match", etag)
						}
						response := httptest.NewRecorder()
						router.ServeHTTP(response, request)
						return response
					}
					activate(currentID)
					current := frontendManagementRequest(t, router, http.MethodGet, url, "")
					require.Equal(t, http.StatusOK, current.Code)
					require.Equal(t, "current asset", current.Body.String())
					activate(previousID)
					previous := requestAsset(current.Header().Get("ETag"))
					require.Equal(t, http.StatusOK, previous.Code, "switching to an older install must invalidate the newer asset")
					require.Equal(t, "previous asset", previous.Body.String())
					require.Equal(t, "no-cache", previous.Header().Get("Cache-Control"))
					previousETag := previous.Header().Get("ETag")
					require.NotEmpty(t, previousETag)
					require.NotEqual(t, current.Header().Get("ETag"), previousETag)
					cached := requestAsset(previousETag)
					require.Equal(t, http.StatusNotModified, cached.Code)
					require.Empty(t, cached.Body.String())
					legacy := requestAsset("")
					require.Equal(t, http.StatusOK, legacy.Code, "legacy timestamp-only cache must be refreshed")
					require.Equal(t, "previous asset", legacy.Body.String())
					activate(currentID)
					restored := requestAsset(previousETag)
					require.Equal(t, http.StatusOK, restored.Code)
					require.Equal(t, "current asset", restored.Body.String())
				})
			}
		})
	}
}

func TestFrontendRemovalProtectsActiveAndBuiltinTemplates(t *testing.T) {
	configPath := frontendManagementFixture(t)
	router := frontendManagementRouter(&model.User{Role: model.RoleAdmin}, nil)
	remove := func(body string, success bool) {
		t.Helper()
		response := frontendManagementRequest(t, router, "POST", "/api/v1/frontend/remove", body)
		var result model.CommonResponse[any]
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result), response.Body.String())
		require.Equal(t, success, result.Success, response.Body.String())
		if !success {
			require.NotEmpty(t, result.Error)
		}
	}
	for _, body := range []string{`{}`, `{"path":"user-dist"}`, `{"path":"admin-dist","is_admin":true}`, `{"path":"missing"}`, `{"path":"../config.yaml"}`, `{"path":"installed-admin"}`, `{"path":"installed-user","is_admin":true}`} {
		remove(body, false)
	}
	for _, role := range []struct {
		path  string
		admin bool
	}{{"installed-user", false}, {"installed-admin", true}} {
		body, err := json.Marshal(frontendActivateForm{Path: role.path, IsAdmin: role.admin})
		require.NoError(t, err)
		response := frontendManagementRequest(t, router, "POST", "/api/v1/frontend/activate", string(body))
		var activation model.CommonResponse[any]
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &activation))
		require.True(t, activation.Success)
		directory := filepath.Join(filepath.Dir(configPath), "frontends", role.path)
		remove(string(body), false)
		require.FileExists(t, filepath.Join(directory, "dist", "index.html"))
		user, admin := "user-dist", "admin-dist"
		require.NoError(t, singleton.Conf.CommitFrontendSelection(&user, &admin, singleton.Conf.Save))
		remove(string(body), true)
		require.NoDirExists(t, directory)
		require.NoError(t, singleton.InitConfigFromPath(configPath))
		for _, template := range singleton.Conf.AvailableFrontendTemplates() {
			require.NotEqual(t, role.path, template.Path)
		}
		response = frontendManagementRequest(t, router, "POST", "/api/v1/frontend/activate", string(body))
		activation = model.CommonResponse[any]{}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &activation))
		require.False(t, activation.Success)
		require.Contains(t, activation.Error, "not installed")
	}
	require.FileExists(t, configPath)
}
