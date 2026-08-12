package singleton

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInitFrontendTemplatesLoadsFutureCatalogEntriesFromConfiguredCache(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "market.json")
	catalog := `{"schema":1,"themes":[{"name":"Future Theme","short":"future-theme","version":"9.1","author":"Future Author","url":"https://github.com/example/future","preview":"https://cdn.example/future.webp","download":"https://github.com/example/future/releases/download/v9.1/theme.zip","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`
	require.NoError(t, os.WriteFile(cache, []byte(catalog), 0o600))
	t.Setenv("NZ_KOMARI_THEME_MARKET_URL", "https://127.0.0.1.invalid/catalog.json")
	t.Setenv("NZ_KOMARI_THEME_MARKET_CACHE", cache)

	originalTemplates := FrontendTemplates
	originalWallpapers := KomariThemeWallpapers
	originalCatalog := KomariThemeCatalog
	t.Cleanup(func() {
		FrontendTemplates = originalTemplates
		KomariThemeWallpapers = originalWallpapers
		KomariThemeCatalog = originalCatalog
	})

	require.NoError(t, InitFrontendTemplates())
	var found bool
	for _, item := range FrontendTemplates {
		if item.Path == "komari-market-future-theme" {
			found = true
			require.Equal(t, "Future Theme", item.Name)
		}
	}
	require.True(t, found)
	require.Equal(t, "https://cdn.example/future.webp", KomariThemeWallpapers["komari-market-future-theme"])
}
