package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/require"
)

func TestKomariAdminPathsRedirectToNezhaDashboard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	original := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{
		AdminTemplate: "admin-dist", UserTemplate: "komari-market-kumo",
	}}}
	singleton.KomariThemeWallpapers = map[string]string{"komari-market-kumo": "https://example.com/p.png"}
	t.Cleanup(func() { singleton.Conf = original; singleton.KomariThemeWallpapers = nil })

	dist := fstest.MapFS{
		"admin-dist/index.html": &fstest.MapFile{Data: []byte("admin index")},
		"user-dist/index.html":  &fstest.MapFile{Data: []byte("user index")},
	}
	r := gin.New()
	r.NoRoute(fallbackToFrontend(dist))

	for _, path := range []string{"/admin", "/admin/", "/admin/login", "/login"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusTemporaryRedirect, rec.Code, path)
		require.Equal(t, "/dashboard", rec.Header().Get("Location"), path)
	}
}

func TestOfficialThemeDoesNotHijackUnrelatedLoginPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	original := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{
		AdminTemplate: "admin-dist", UserTemplate: "user-dist",
	}}}
	t.Cleanup(func() { singleton.Conf = original })
	dist := fstest.MapFS{"admin-dist/index.html": &fstest.MapFile{Data: []byte("admin")}, "user-dist/index.html": &fstest.MapFile{Data: []byte("user")}}
	r := gin.New()
	r.NoRoute(fallbackToFrontend(dist))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	require.NotEqual(t, http.StatusTemporaryRedirect, rec.Code)
}
