package singleton

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func themeZipFixture(t *testing.T, files map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func TestInstallKomariThemeArchiveVerifiesDigestAndExtractsDist(t *testing.T) {
	data, digest := themeZipFixture(t, map[string]string{
		"dist/index.html":    `<html><head><script src="/assets/app.js"></script></head></html>`,
		"dist/assets/app.js": `window.realKomariTheme=true`,
		"komari-theme.json":  `{"short":"future"}`,
	})
	root := t.TempDir()
	path, err := installKomariThemeArchive(root, "komari-market-future", KomariThemeMetadata{Short: "future", SHA256: digest, Version: "1"}, data)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "komari-market-future", "dist"), path)
	require.FileExists(t, filepath.Join(path, "index.html"))
	require.FileExists(t, filepath.Join(path, "assets", "app.js"))
	require.FileExists(t, filepath.Join(root, "komari-market-future", ".nezha-komari-version"))
}

func TestInstallKomariThemeArchiveRejectsDigestMismatch(t *testing.T) {
	data, _ := themeZipFixture(t, map[string]string{"dist/index.html": "ok"})
	_, err := installKomariThemeArchive(t.TempDir(), "komari-market-bad", KomariThemeMetadata{Short: "bad", SHA256: string(make([]byte, 64))}, data)
	require.ErrorContains(t, err, "sha256")
}

func TestInstallKomariThemeArchiveRejectsTraversalAndSymlinks(t *testing.T) {
	data, digest := themeZipFixture(t, map[string]string{"../escape": "bad", "dist/index.html": "ok"})
	_, err := installKomariThemeArchive(t.TempDir(), "komari-market-bad", KomariThemeMetadata{Short: "bad", SHA256: digest}, data)
	require.ErrorContains(t, err, "unsafe")
}

func TestInstallKomariThemeArchiveRequiresDistIndex(t *testing.T) {
	data, digest := themeZipFixture(t, map[string]string{"komari-theme.json": "{}"})
	_, err := installKomariThemeArchive(t.TempDir(), "komari-market-bad", KomariThemeMetadata{Short: "bad", SHA256: digest}, data)
	require.ErrorContains(t, err, "dist/index.html")
}

func TestSyncKomariThemesToleratesNilConfig(t *testing.T) {
	original := Conf
	Conf = nil
	t.Cleanup(func() { Conf = original })
	require.NotPanics(t, syncKomariThemes)
}

func TestResolveKomariFrontendTemplateUsesInstalledThemeDist(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NZ_KOMARI_THEME_DIR", root)
	path := filepath.Join(root, "komari-market-future", "dist")
	require.NoError(t, os.MkdirAll(path, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(path, "index.html"), []byte("real theme"), 0o644))
	KomariThemeCatalog = map[string]KomariThemeMetadata{"komari-market-future": {Short: "future", Preview: "https://cdn.example/p.png"}}
	KomariThemeWallpapers = map[string]string{"komari-market-future": "https://cdn.example/p.png"}
	t.Cleanup(func() { KomariThemeCatalog = nil; KomariThemeWallpapers = nil })

	got, wallpaper, ok := ResolveKomariFrontendTemplate("komari-market-future")
	require.True(t, ok)
	require.Equal(t, path, got)
	require.Equal(t, "https://cdn.example/p.png", wallpaper)
}
