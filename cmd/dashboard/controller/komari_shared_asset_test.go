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

func TestKomariSharedBackendAssetsCanComeFromAnotherVerifiedTheme(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	t.Setenv("NZ_KOMARI_THEME_DIR", root)
	selected := filepath.Join(root, "komari-market-emerald", "dist")
	provider := filepath.Join(root, "komari-market-material", "dist", "images", "logo")
	require.NoError(t, os.MkdirAll(selected, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(selected, "index.html"), []byte("emerald"), 0o644))
	require.NoError(t, os.MkdirAll(provider, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(provider, "linux.svg"), []byte("shared-linux"), 0o644))
	original := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{AdminTemplate: "admin-dist", UserTemplate: "komari-market-emerald"}}}
	singleton.KomariThemeCatalog = map[string]singleton.KomariThemeMetadata{"komari-market-emerald": {Short: "emerald", Preview: "https://example.com/p.png"}}
	singleton.KomariThemeWallpapers = map[string]string{"komari-market-emerald": "https://example.com/p.png"}
	t.Cleanup(func() {
		singleton.Conf = original
		singleton.KomariThemeCatalog = nil
		singleton.KomariThemeWallpapers = nil
	})
	embed := fstest.MapFS{"admin-dist/index.html": &fstest.MapFile{Data: []byte("admin")}, "user-dist/index.html": &fstest.MapFile{Data: []byte("official")}}
	r := gin.New()
	r.NoRoute(fallbackToFrontend(embed))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/logo/linux.svg", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "shared-linux", rec.Body.String())
	require.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
}

func TestKomariSharedBackendAssetLookupCannotEscapeThemeDirectory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	t.Setenv("NZ_KOMARI_THEME_DIR", root)
	selected := filepath.Join(root, "komari-market-emerald", "dist")
	require.NoError(t, os.MkdirAll(selected, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(selected, "index.html"), []byte("emerald"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret"), []byte("secret"), 0o600))
	original := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{AdminTemplate: "admin-dist", UserTemplate: "komari-market-emerald"}}}
	singleton.KomariThemeCatalog = map[string]singleton.KomariThemeMetadata{"komari-market-emerald": {Short: "emerald", Preview: "https://example.com/p.png"}}
	singleton.KomariThemeWallpapers = map[string]string{"komari-market-emerald": "https://example.com/p.png"}
	t.Cleanup(func() {
		singleton.Conf = original
		singleton.KomariThemeCatalog = nil
		singleton.KomariThemeWallpapers = nil
	})
	embed := fstest.MapFS{"admin-dist/index.html": &fstest.MapFile{Data: []byte("admin")}, "user-dist/index.html": &fstest.MapFile{Data: []byte("official")}}
	r := gin.New()
	r.NoRoute(fallbackToFrontend(embed))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/logo/../../../secret", nil))
	require.NotEqual(t, "secret", rec.Body.String())
}
