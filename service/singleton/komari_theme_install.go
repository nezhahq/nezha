package singleton

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxKomariThemeArchiveBytes = 128 << 20
	maxKomariThemeExtractBytes = 512 << 20
	maxKomariThemeFiles        = 10000
)

func komariThemeDir() string {
	if root := strings.TrimSpace(os.Getenv("NZ_KOMARI_THEME_DIR")); root != "" {
		return root
	}
	return filepath.Join("data", "komari-themes")
}

func KomariThemeInstallDir() string { return komariThemeDir() }

func installedKomariThemeDist(templatePath string) (string, bool) {
	if !strings.HasPrefix(templatePath, "komari-market-") || komariTemplatePath(strings.TrimPrefix(templatePath, "komari-market-")) != templatePath {
		return "", false
	}
	dist := filepath.Join(komariThemeDir(), templatePath, "dist")
	info, err := os.Stat(filepath.Join(dist, "index.html"))
	return dist, err == nil && !info.IsDir()
}

func installKomariThemeArchive(root, templatePath string, meta KomariThemeMetadata, data []byte) (string, error) {
	if len(data) == 0 || len(data) > maxKomariThemeArchiveBytes {
		return "", fmt.Errorf("theme archive size invalid")
	}
	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), meta.SHA256) {
		return "", fmt.Errorf("theme archive sha256 mismatch")
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	if len(zr.File) > maxKomariThemeFiles {
		return "", fmt.Errorf("theme archive has too many files")
	}
	target := filepath.Join(root, templatePath)
	tmp, err := os.MkdirTemp(root, ".komari-install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	var total uint64
	for _, f := range zr.File {
		name := filepath.Clean(filepath.FromSlash(f.Name))
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) || f.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("unsafe theme archive path %q", f.Name)
		}
		total += f.UncompressedSize64
		if total > maxKomariThemeExtractBytes {
			return "", fmt.Errorf("theme archive extracted size exceeds limit")
		}
		out := filepath.Join(tmp, name)
		rel, err := filepath.Rel(tmp, out)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return "", fmt.Errorf("unsafe theme archive path %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(out, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return "", err
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		dst, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return "", err
		}
		_, copyErr := io.Copy(dst, io.LimitReader(rc, int64(f.UncompressedSize64)+1))
		closeErr := dst.Close()
		rc.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	if info, err := os.Stat(filepath.Join(tmp, "dist", "index.html")); err != nil || info.IsDir() {
		return "", fmt.Errorf("theme archive missing dist/index.html")
	}
	if err := os.WriteFile(filepath.Join(tmp, ".nezha-komari-version"), []byte(meta.Version+"\n"+meta.SHA256+"\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", err
	}
	return filepath.Join(target, "dist"), nil
}

func ensureKomariThemeInstalled(templatePath string) (string, error) {
	komariThemeMu.RLock()
	meta, ok := KomariThemeCatalog[templatePath]
	komariThemeMu.RUnlock()
	if !ok {
		return "", fmt.Errorf("unknown Komari theme")
	}
	if dist, ok := installedKomariThemeDist(templatePath); ok {
		marker, err := os.ReadFile(filepath.Join(komariThemeDir(), templatePath, ".nezha-komari-version"))
		if err == nil && strings.Contains(string(marker), meta.SHA256) {
			return dist, nil
		}
	}
	client := &http.Client{Timeout: 45 * time.Second}
	req, err := http.NewRequest(http.MethodGet, meta.Download, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Nezha-Dashboard-Komari-Theme/1")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("theme download HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxKomariThemeArchiveBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxKomariThemeArchiveBytes {
		return "", fmt.Errorf("theme archive exceeds limit")
	}
	if err := os.MkdirAll(komariThemeDir(), 0o755); err != nil {
		return "", err
	}
	return installKomariThemeArchive(komariThemeDir(), templatePath, meta, data)
}

func syncKomariThemes() {
	// Tests and restricted startup paths can initialize the template catalog
	// before the dashboard config exists.
	if Conf == nil || Conf.Config == nil {
		return
	}
	// Install only the currently selected theme. Installing the entire market at
	// startup wastes bandwidth and expands the third-party code surface.
	selected := strings.TrimSpace(Conf.UserTemplate)
	if !strings.HasPrefix(selected, "komari-market-") {
		return
	}
	if _, err := ensureKomariThemeInstalled(selected); err != nil {
		log.Printf("NEZHA>> Komari theme %s install failed; using official fallback: %v", selected, err)
	}
}
