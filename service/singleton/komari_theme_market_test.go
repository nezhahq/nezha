package singleton

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseKomariThemeMarketCreatesTemplatesWithoutHardcodedNames(t *testing.T) {
	catalog := `{"schema":1,"themes":[
		{"name":{"zh-CN":"未来主题","en":"Future Theme"},"short":"future-theme","version":"9.1.0","author":{"en":"Future Author"},"url":"https://github.com/example/future","preview":"https://cdn.example/future.webp","download":"https://github.com/example/future/releases/download/v9.1.0/theme.zip","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"name":"Second Theme","short":"SECOND Theme","version":"1.0","author":"Author","url":"https://github.com/example/second","preview":"https://cdn.example/second.png","download":"https://github.com/example/second/releases/download/v1/theme.zip","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	]}`

	templates, themes, err := parseKomariThemeMarket(strings.NewReader(catalog))
	require.NoError(t, err)
	require.Len(t, templates, 2)
	byPath := make(map[string]struct {
		name      string
		wallpaper string
	})
	for _, item := range templates {
		byPath[item.Path] = struct {
			name      string
			wallpaper string
		}{item.Name, themes[item.Path].Preview}
	}
	require.Equal(t, "未来主题", byPath["komari-market-future-theme"].name)
	require.Equal(t, "https://cdn.example/future.webp", byPath["komari-market-future-theme"].wallpaper)
	require.Contains(t, byPath, "komari-market-second-theme")
}

func TestParseKomariThemeMarketRejectsUnsafeOrIncompleteEntries(t *testing.T) {
	catalog := `{"schema":1,"themes":[
		{"name":"Unsafe","short":"unsafe","version":"1","author":"x","url":"http://example.com/repo","preview":"javascript:alert(1)","download":"https://example.com/a.zip","sha256":"bad"},
		{"name":"Valid","short":"valid","version":"1","author":"x","url":"https://github.com/example/valid","preview":"https://cdn.example/valid.webp","download":"https://github.com/example/valid/releases/download/v1/a.zip","sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}
	]}`

	templates, themes, err := parseKomariThemeMarket(strings.NewReader(catalog))
	require.NoError(t, err)
	require.Len(t, templates, 1)
	require.Equal(t, "komari-market-valid", templates[0].Path)
	require.Len(t, themes, 1)
}

func TestParseKomariThemeMarketPreservesDownloadAndDigestForRuntimeInstall(t *testing.T) {
	catalog := `{"schema":1,"themes":[{"name":"Future","short":"future","version":"1","author":"A","url":"https://github.com/example/future","preview":"https://cdn.example/preview.png","download":"https://github.com/example/future/releases/download/v1/theme.zip","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`
	templates, themes, err := parseKomariThemeMarket(strings.NewReader(catalog))
	require.NoError(t, err)
	require.Len(t, templates, 1)
	meta := themes[templates[0].Path]
	require.Equal(t, "https://github.com/example/future/releases/download/v1/theme.zip", meta.Download)
	require.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", meta.SHA256)
	require.Equal(t, "future", meta.Short)
}

func TestKomariTemplateResolutionUsesOfficialFrontendAndCatalogWallpaper(t *testing.T) {
	KomariThemeWallpapers = map[string]string{"komari-market-future": "https://cdn.example/future.webp"}
	t.Cleanup(func() { KomariThemeWallpapers = nil })

	root, wallpaper, ok := ResolveKomariFrontendTemplate("komari-market-future")
	require.True(t, ok)
	require.Equal(t, "user-dist", root)
	require.Equal(t, "https://cdn.example/future.webp", wallpaper)

	_, _, ok = ResolveKomariFrontendTemplate("user-dist")
	require.False(t, ok)
}

func TestInjectKomariWallpaperLocksOnlyBackgroundVariables(t *testing.T) {
	html := []byte(`<html><head></head><body></body></html>`)
	out := string(injectKomariWallpaper(html, "https://cdn.example/future.webp", "komari-market-future"))
	require.Contains(t, out, "Object.defineProperty(window,'CustomBackgroundImage'")
	require.Contains(t, out, "https://cdn.example/future.webp")
	require.Contains(t, out, "komari-market-future")
	require.NotContains(t, out, "DisableAnimatedMan")
}
