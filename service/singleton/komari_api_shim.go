package singleton

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

//go:embed komari-api-shim.js
var komariAPIShim []byte

func KomariAPIShimJS() []byte {
	return append([]byte(nil), komariAPIShim...)
}

func InjectKomariCompatibility(document []byte, templatePath, wallpaper string) []byte {
	config, _ := json.Marshal(map[string]any{
		"short":          strings.TrimPrefix(templatePath, "komari-market-"),
		"template_path":  templatePath,
		"wallpaper":      wallpaper,
		"theme_settings": map[string]any{},
	})
	shim := `<script id="nezha-komari-api-shim">window.__NEZHA_KOMARI_COMPAT__=` + string(config) + `;</script><script>` + string(komariAPIShim) + `</script>`
	text := string(document)
	if pos := strings.Index(strings.ToLower(text), "<script"); pos >= 0 {
		return []byte(text[:pos] + shim + text[pos:])
	}
	if pos := strings.Index(strings.ToLower(text), "</head>"); pos >= 0 {
		return []byte(text[:pos] + shim + text[pos:])
	}
	return append([]byte(shim), document...)
}

type komariShimServerFixture struct {
	ID         uint64    `json:"id"`
	Name       string    `json:"name"`
	LastActive time.Time `json:"last_active"`
	Host       struct {
		Platform       string   `json:"platform"`
		CPU            []string `json:"cpu"`
		MemTotal       int64    `json:"mem_total"`
		DiskTotal      int64    `json:"disk_total"`
		SwapTotal      int64    `json:"swap_total"`
		Arch           string   `json:"arch"`
		Virtualization string   `json:"virtualization"`
	} `json:"host"`
	State struct {
		CPU            float64 `json:"cpu"`
		MemUsed        int64   `json:"mem_used"`
		NetOutTransfer int64   `json:"net_out_transfer"`
	} `json:"state"`
}

type KomariShimFixtureResult struct {
	UUID     string
	Name     string
	OS       string
	MemTotal int64
	Status   struct {
		CPU        float64
		RAM        int64
		NetTotalUp int64
		Online     bool
	}
}

func KomariShimConvertServerFixture(raw []byte) (KomariShimFixtureResult, error) {
	var src komariShimServerFixture
	if err := json.Unmarshal(raw, &src); err != nil {
		return KomariShimFixtureResult{}, err
	}
	var out KomariShimFixtureResult
	out.UUID = fmt.Sprint(src.ID)
	out.Name = src.Name
	out.OS = src.Host.Platform
	out.MemTotal = src.Host.MemTotal
	out.Status.CPU = src.State.CPU
	out.Status.RAM = src.State.MemUsed
	out.Status.NetTotalUp = src.State.NetOutTransfer
	out.Status.Online = time.Since(src.LastActive) < 3*time.Minute
	return out, nil
}
