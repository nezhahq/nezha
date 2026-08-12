package controller

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/require"
)

func TestKomariThemeAssetsResolveCaseInsensitivePathSegments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	t.Setenv("NZ_KOMARI_THEME_DIR", root)
	distRoot := filepath.Join(root, "komari-market-material", "dist")
	require.NoError(t, os.MkdirAll(filepath.Join(distRoot, "images", "flags"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(distRoot, "index.html"), []byte("material"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(distRoot, "images", "flags", "CA.svg"), []byte("canada"), 0o644))

	original := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{AdminTemplate: "admin-dist", UserTemplate: "komari-market-material"}}}
	singleton.KomariThemeCatalog = map[string]singleton.KomariThemeMetadata{"komari-market-material": {Short: "material", Preview: "https://example.com/p.png"}}
	singleton.KomariThemeWallpapers = map[string]string{"komari-market-material": "https://example.com/p.png"}
	t.Cleanup(func() {
		singleton.Conf = original
		singleton.KomariThemeCatalog = nil
		singleton.KomariThemeWallpapers = nil
	})

	embed := fstest.MapFS{"admin-dist/index.html": &fstest.MapFile{Data: []byte("admin")}, "user-dist/index.html": &fstest.MapFile{Data: []byte("official")}}
	r := gin.New()
	r.NoRoute(fallbackToFrontend(embed))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/images/flags/ca.svg", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "canada", rec.Body.String())
}

func TestKomariThemeMissingAssetStillReturnsNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	t.Setenv("NZ_KOMARI_THEME_DIR", root)
	distRoot := filepath.Join(root, "komari-market-material", "dist")
	require.NoError(t, os.MkdirAll(distRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(distRoot, "index.html"), []byte("material"), 0o644))
	original := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{AdminTemplate: "admin-dist", UserTemplate: "komari-market-material"}}}
	singleton.KomariThemeCatalog = map[string]singleton.KomariThemeMetadata{"komari-market-material": {Short: "material", Preview: "https://example.com/p.png"}}
	singleton.KomariThemeWallpapers = map[string]string{"komari-market-material": "https://example.com/p.png"}
	t.Cleanup(func() {
		singleton.Conf = original
		singleton.KomariThemeCatalog = nil
		singleton.KomariThemeWallpapers = nil
	})
	embed := fstest.MapFS{"admin-dist/index.html": &fstest.MapFile{Data: []byte("admin")}, "user-dist/index.html": &fstest.MapFile{Data: []byte("official")}}
	r := gin.New()
	r.NoRoute(fallbackToFrontend(embed))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/images/flags/no-such.svg", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}
