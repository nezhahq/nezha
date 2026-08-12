package singleton

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nezhahq/nezha/model"
)

const defaultKomariThemeMarketURL = "https://raw.githubusercontent.com/komari-monitor/theme-market/main/v1.json"

//go:embed komari-theme-market-v1.json
var embeddedKomariThemeMarket []byte

var (
	komariThemeMu         sync.RWMutex
	KomariThemeWallpapers map[string]string
	KomariThemeCatalog    map[string]KomariThemeMetadata
)

type KomariThemeMetadata struct {
	Short    string
	Preview  string
	Download string
	SHA256   string
	Version  string
}

type localizedText any

type komariMarketCatalog struct {
	Schema int `json:"schema"`
	Themes []struct {
		Name     localizedText `json:"name"`
		Short    string        `json:"short"`
		Version  string        `json:"version"`
		Author   localizedText `json:"author"`
		URL      string        `json:"url"`
		Preview  string        `json:"preview"`
		Download string        `json:"download"`
		SHA256   string        `json:"sha256"`
	} `json:"themes"`
}

var safeThemePart = regexp.MustCompile(`[^a-z0-9]+`)
var sha256Hex = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

func localizedString(value localizedText) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		for _, key := range []string{"zh-CN", "zh-TW", "en", "ja"} {
			if text, ok := v[key].(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
		for _, raw := range v {
			if text, ok := raw.(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

func komariTemplatePath(short string) string {
	part := strings.Trim(safeThemePart.ReplaceAllString(strings.ToLower(strings.TrimSpace(short)), "-"), "-")
	if part == "" {
		return ""
	}
	return "komari-market-" + part
}

func parseKomariThemeMarket(r io.Reader) ([]model.FrontendTemplate, map[string]KomariThemeMetadata, error) {
	dec := json.NewDecoder(io.LimitReader(r, 2<<20))
	dec.UseNumber()
	var catalog komariMarketCatalog
	if err := dec.Decode(&catalog); err != nil {
		return nil, nil, err
	}
	if catalog.Schema != 1 {
		return nil, nil, fmt.Errorf("unsupported Komari theme market schema %d", catalog.Schema)
	}

	seen := make(map[string]struct{})
	templates := make([]model.FrontendTemplate, 0, len(catalog.Themes))
	themes := make(map[string]KomariThemeMetadata)
	for _, theme := range catalog.Themes {
		path := komariTemplatePath(theme.Short)
		name := localizedString(theme.Name)
		author := localizedString(theme.Author)
		if path == "" || name == "" || author == "" || theme.Version == "" ||
			!httpsURL(theme.URL) || !httpsURL(theme.Preview) || !httpsURL(theme.Download) ||
			!sha256Hex.MatchString(theme.SHA256) {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		templates = append(templates, model.FrontendTemplate{
			Path: path, Name: name, Repository: theme.URL, Author: author, Version: theme.Version,
		})
		themes[path] = KomariThemeMetadata{Short: theme.Short, Preview: theme.Preview, Download: theme.Download, SHA256: strings.ToLower(theme.SHA256), Version: theme.Version}
	}
	sort.SliceStable(templates, func(i, j int) bool { return templates[i].Name < templates[j].Name })
	return templates, themes, nil
}

func komariMarketCachePath() string {
	if path := strings.TrimSpace(os.Getenv("NZ_KOMARI_THEME_MARKET_CACHE")); path != "" {
		return path
	}
	return filepath.Join("data", "komari-theme-market-v1.json")
}

func fetchKomariThemeMarket() ([]byte, error) {
	marketURL := strings.TrimSpace(os.Getenv("NZ_KOMARI_THEME_MARKET_URL"))
	if marketURL == "" {
		marketURL = defaultKomariThemeMarketURL
	}
	if !httpsURL(marketURL) {
		return nil, fmt.Errorf("Komari theme market URL must use HTTPS")
	}
	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequest(http.MethodGet, marketURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Nezha-Dashboard-Komari-Catalog/1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("theme market HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 2<<20 {
		return nil, fmt.Errorf("theme market exceeds 2 MiB")
	}
	return data, nil
}

func loadKomariThemeMarket() ([]model.FrontendTemplate, map[string]KomariThemeMetadata, string, error) {
	if data, err := fetchKomariThemeMarket(); err == nil {
		if templates, wallpapers, parseErr := parseKomariThemeMarket(strings.NewReader(string(data))); parseErr == nil && len(templates) > 0 {
			cache := komariMarketCachePath()
			if mkErr := os.MkdirAll(filepath.Dir(cache), 0o755); mkErr == nil {
				if writeErr := os.WriteFile(cache, data, 0o644); writeErr != nil {
					log.Printf("NEZHA>> write Komari market cache: %v", writeErr)
				}
			}
			return templates, wallpapers, "remote", nil
		}
	}
	if data, err := os.ReadFile(komariMarketCachePath()); err == nil {
		if templates, wallpapers, parseErr := parseKomariThemeMarket(strings.NewReader(string(data))); parseErr == nil && len(templates) > 0 {
			return templates, wallpapers, "cache", nil
		}
	}
	templates, wallpapers, err := parseKomariThemeMarket(strings.NewReader(string(embeddedKomariThemeMarket)))
	return templates, wallpapers, "embedded", err
}

func appendKomariThemeMarketTemplates(base []model.FrontendTemplate) []model.FrontendTemplate {
	templates, wallpapers, source, err := loadKomariThemeMarket()
	if err != nil {
		log.Printf("NEZHA>> Komari theme market unavailable: %v", err)
		return base
	}
	komariThemeMu.Lock()
	KomariThemeCatalog = wallpapers
	KomariThemeWallpapers = make(map[string]string, len(wallpapers))
	for path, theme := range wallpapers {
		KomariThemeWallpapers[path] = theme.Preview
	}
	komariThemeMu.Unlock()
	log.Printf("NEZHA>> Loaded %d Komari wallpaper templates from %s catalog", len(templates), source)
	return append(base, templates...)
}

// ResolveKomariFrontendTemplate maps a dynamic catalog item to the official Nezha frontend.
// Komari packages target a different API, so only their catalog preview is used as wallpaper.
func ResolveKomariFrontendTemplate(path string) (templateRoot, wallpaper string, ok bool) {
	komariThemeMu.RLock()
	wallpaper, ok = KomariThemeWallpapers[path]
	komariThemeMu.RUnlock()
	if !ok {
		return "", "", false
	}
	if dist, installed := installedKomariThemeDist(path); installed {
		return dist, wallpaper, true
	}
	return "user-dist", wallpaper, true
}

func InjectKomariWallpaper(document []byte, wallpaper, templatePath string) []byte {
	return injectKomariWallpaper(document, wallpaper, templatePath)
}

func injectKomariWallpaper(document []byte, wallpaper, templatePath string) []byte {
	if !httpsURL(wallpaper) {
		return document
	}
	script := `<script id="nezha-komari-market-wallpaper">;(()=>{const wallpaper=` +
		fmt.Sprintf("%q", wallpaper) + `;Object.defineProperty(window,'CustomBackgroundImage',{configurable:false,enumerable:true,writable:false,value:wallpaper});Object.defineProperty(window,'CustomMobileBackgroundImage',{configurable:false,enumerable:true,writable:false,value:wallpaper});window.__NEZHA_KOMARI_MARKET_THEME__=` +
		fmt.Sprintf("%q", html.EscapeString(templatePath)) + `;})();</script>`
	text := string(document)
	if pos := strings.Index(text, "</head>"); pos >= 0 {
		return []byte(text[:pos] + script + text[pos:])
	}
	return append([]byte(script), document...)
}
