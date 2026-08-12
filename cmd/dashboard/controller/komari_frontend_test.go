package controller

import (
	"io/fs"
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

func TestResolveFrontendTemplateMapsDynamicKomariEntryToOfficialDist(t *testing.T) {
	singleton.KomariThemeWallpapers = map[string]string{
		"komari-market-future": "https://cdn.example/future.webp",
	}
	t.Cleanup(func() { singleton.KomariThemeWallpapers = nil })

	root, wallpaper := resolveUserFrontendTemplate("komari-market-future")
	require.Equal(t, "user-dist", root)
	require.Equal(t, "https://cdn.example/future.webp", wallpaper)

	root, wallpaper = resolveUserFrontendTemplate("nazhua-dist")
	require.Equal(t, "nazhua-dist", root)
	require.Empty(t, wallpaper)
}

func TestPrepareFrontendIndexInjectsDynamicKomariWallpaper(t *testing.T) {
	raw := []byte(`<html><head></head><body></body></html>`)
	out := string(prepareFrontendIndex(raw, "https://cdn.example/future.webp", "komari-market-future"))
	require.Contains(t, out, "CustomBackgroundImage")
	require.Contains(t, out, "https://cdn.example/future.webp")
	require.Contains(t, out, "komari-market-future")
}

func TestFallbackServesKomariManifestCompatibilityPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	t.Setenv("NZ_KOMARI_THEME_DIR", root)
	themeRoot := filepath.Join(root, "komari-market-future")
	require.NoError(t, os.MkdirAll(filepath.Join(themeRoot, "dist"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(themeRoot, "dist", "index.html"), []byte("theme"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(themeRoot, "komari-theme.json"), []byte(`{"short":"future","configuration":{"backgroundImage":{"default":"x"}}}`), 0o644))
	original := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{AdminTemplate: "admin-dist", UserTemplate: "komari-market-future"}}}
	singleton.KomariThemeCatalog = map[string]singleton.KomariThemeMetadata{"komari-market-future": {Short: "future", Preview: "https://example.com/p.png"}}
	singleton.KomariThemeWallpapers = map[string]string{"komari-market-future": "https://example.com/p.png"}
	t.Cleanup(func() {
		singleton.Conf = original
		singleton.KomariThemeCatalog = nil
		singleton.KomariThemeWallpapers = nil
	})
	embed := fstest.MapFS{"admin-dist/index.html": &fstest.MapFile{Data: []byte("admin")}, "user-dist/index.html": &fstest.MapFile{Data: []byte("official")}}
	r := gin.New()
	r.NoRoute(fallbackToFrontend(embed))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/themes/future/komari-theme.json", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"short":"future","configuration":{"backgroundImage":{"default":"x"}}}`, rec.Body.String())
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
}

func TestFallbackServesInstalledKomariThemeAssetsInsteadOfOfficialAssets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	t.Setenv("NZ_KOMARI_THEME_DIR", root)
	themeDist := filepath.Join(root, "komari-market-future", "dist")
	require.NoError(t, os.MkdirAll(filepath.Join(themeDist, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(themeDist, "index.html"), []byte(`<html><head><script src="/assets/komari.js"></script></head><body id="real-komari"></body></html>`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(themeDist, "assets", "komari.js"), []byte(`window.realKomariTheme=true`), 0o644))

	originalConf := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{AdminTemplate: "admin-dist", UserTemplate: "komari-market-future"}}}
	singleton.KomariThemeCatalog = map[string]singleton.KomariThemeMetadata{"komari-market-future": {Short: "future", Preview: "https://cdn.example/future.webp"}}
	singleton.KomariThemeWallpapers = map[string]string{"komari-market-future": "https://cdn.example/future.webp"}
	t.Cleanup(func() {
		singleton.Conf = originalConf
		singleton.KomariThemeCatalog = nil
		singleton.KomariThemeWallpapers = nil
	})

	dist := fstest.MapFS{"user-dist/index.html": &fstest.MapFile{Data: []byte(`<html><body id="official"></body></html>`)}, "admin-dist/index.html": &fstest.MapFile{Data: []byte(`admin`)}}
	r := gin.New()
	r.NoRoute(fallbackToFrontend(dist))

	indexRec := httptest.NewRecorder()
	r.ServeHTTP(indexRec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, indexRec.Code)
	require.Contains(t, indexRec.Body.String(), `real-komari`)
	require.NotContains(t, indexRec.Body.String(), `id="official"`)
	require.Contains(t, indexRec.Body.String(), `nezha-komari-api-shim`)

	assetRec := httptest.NewRecorder()
	r.ServeHTTP(assetRec, httptest.NewRequest(http.MethodGet, "/assets/komari.js", nil))
	require.Equal(t, http.StatusOK, assetRec.Code)
	require.Equal(t, `window.realKomariTheme=true`, assetRec.Body.String())
}

func TestFallbackServesOfficialAssetsAndInjectedIndexForDynamicTheme(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalConf := singleton.Conf
	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{
		AdminTemplate: "admin-dist", UserTemplate: "komari-market-future",
	}}}
	singleton.KomariThemeWallpapers = map[string]string{
		"komari-market-future": "https://cdn.example/future.webp",
	}
	t.Cleanup(func() {
		singleton.Conf = originalConf
		singleton.KomariThemeWallpapers = nil
	})

	dist := fstest.MapFS{
		"user-dist/index.html":    &fstest.MapFile{Data: []byte(`<html><head></head><body>official</body></html>`)},
		"user-dist/assets/app.js": &fstest.MapFile{Data: []byte(`console.log("official")`)},
		"admin-dist/index.html":   &fstest.MapFile{Data: []byte(`<html><body>admin</body></html>`)},
	}
	var frontend fs.FS = dist
	r := gin.New()
	r.NoRoute(fallbackToFrontend(frontend))

	indexRec := httptest.NewRecorder()
	r.ServeHTTP(indexRec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, indexRec.Code)
	require.Contains(t, indexRec.Body.String(), "https://cdn.example/future.webp")
	require.Contains(t, indexRec.Body.String(), "official")

	assetRec := httptest.NewRecorder()
	r.ServeHTTP(assetRec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	require.Equal(t, http.StatusOK, assetRec.Code)
	require.Equal(t, `console.log("official")`, assetRec.Body.String())
}
