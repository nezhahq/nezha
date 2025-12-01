package geoip

import (
	_ "embed"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"

	maxminddb "github.com/oschwald/maxminddb-golang"

	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

//====================
// 1. 内置 geoip.db（embed）
//====================

//go:embed geoip.db
var embeddedDB []byte

//====================
// 2. 外部 mmdb（路径从配置读取）+ sync.OnceValues
//====================

// 从全局配置读取外部 mmdb 路径：
// - 配置项：ConfigDashboard.GeoIPDBPath（config.yaml 中的 geoip_db_path）
// - 未配置或为空：返回 ""，表示跳过外部数据库，直接使用内置 geoip.db
func getExternalDBPath() string {
	if singleton.Conf == nil {
		return ""
	}

	path := strings.TrimSpace(singleton.Conf.GeoIPDBPath)
	if path == "" {
		return ""
	}
	return path
}

// 使用 sync.OnceValues，一次初始化并缓存 (*maxminddb.Reader, error)
var dbOnce = sync.OnceValues(func() (*maxminddb.Reader, error) {
	// 先看配置是否指定了外部 mmdb
	dbPath := getExternalDBPath()

	// 如果有配置路径，就尝试加载外部 mmdb
	if dbPath != "" {
		if info, err := os.Stat(dbPath); err == nil && !info.IsDir() {
			if reader, err := maxminddb.Open(dbPath); err == nil {
				return reader, nil
			}
			// 打不开就继续走内置数据库
		}
	}

	// 外部未配置或失败 → 回退到内置 geoip.db
	return maxminddb.FromBytes(embeddedDB)
})

// getDB 只是对 dbOnce 的封装，方便后面调用
func getDB() (*maxminddb.Reader, error) {
	return dbOnce()
}

// 适配两种 mmdb 格式：
// - 内置 geoip.db：country/continent 是代码，country_name/continent_name 是名字
// - 外部 ipinfo_lite / country.mmdb：country/country_name 是名字，country_code/continent_code 是代码
type IPInfo struct {
	CountryCode   string `maxminddb:"country_code"`
	Country       string `maxminddb:"country"`
	CountryName   string `maxminddb:"country_name"`
	ContinentCode string `maxminddb:"continent_code"`
	Continent     string `maxminddb:"continent"`
	ContinentName string `maxminddb:"continent_name"`
}

//====================
// 3. ipinfo.io 远程查询（Token 从配置读取）
//====================

// 从配置里读 ipinfo token：ConfigDashboard.IPInfoToken（config.yaml: ipinfo_token）
func getIPInfoToken() string {
	if singleton.Conf == nil {
		return ""
	}
	return strings.TrimSpace(singleton.Conf.IPInfoToken)
}

// 通用的 HTTP 请求函数：访问 url，期望返回 2 位国家码
// 这里复用 utils.HttpClient
func fetchIPInfoCountry(url string) (string, error) {
	if utils.HttpClient == nil {
		return "", errors.New("utils.HttpClient is nil")
	}

	resp, err := utils.HttpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", errors.New("ipinfo status not OK")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	code := strings.TrimSpace(string(body))

	// 简化判断：只要不是 2 个字符，就认为失败
	if len(code) != 2 {
		return "", errors.New("invalid country code from ipinfo")
	}

	// 统一转成小写返回，例如 "HK" -> "hk"
	return strings.ToLower(code), nil
}

// 尝试通过 ipinfo 获取国家代码：
// - 仅在配置了 ipinfo_token 时才会调用
// - 没有 token 时直接返回错误，让上层走本地 mmdb 逻辑
func lookupFromIPInfo(ip net.IP) (string, error) {
	if ip == nil {
		return "", errors.New("nil ip")
	}

	token := getIPInfoToken()
	if token == "" {
		// 没有配置 token，跳过 ipinfo 查询
		return "", errors.New("ipinfo token not set")
	}

	url := "https://ipinfo.io/" + ip.String() + "/country?token=" + token
	return fetchIPInfoCountry(url)
}

//====================
// 4. 对外暴露的 Lookup
//====================

// Lookup 返回小写 2 位代码：
// 优先 ipinfo.io(country)，失败则使用 mmdb（外部或内置）
func Lookup(ip net.IP) (string, error) {
	// 1) 优先用 ipinfo.io（仅在配置了 token 时生效）
	if code, err := lookupFromIPInfo(ip); err == nil && code != "" {
		// code 已经是 2 位小写，如 hk、us、cn
		return code, nil
	}

	// 2) 外部调用失败 → 回退到本地 mmdb 逻辑
	db, err := getDB()
	if err != nil {
		return "", err
	}

	var record IPInfo
	if err := db.Lookup(ip, &record); err != nil {
		return "", err
	}

	// ==== 国家码优先级 ====
	// 1) 优先 country_code（适配你外部化的 mmdb：country_code: AQ）
	if record.CountryCode != "" {
		return strings.ToLower(record.CountryCode), nil
	}
	// 2) 其次 country 刚好是 2 位（适配内置 mmdb 把代码放在 country 的情况：country: AQ）
	if record.Country != "" && len(record.Country) == 2 {
		return strings.ToLower(record.Country), nil
	}

	// ==== 洲码兜底（极端情况用洲代码）====
	if record.ContinentCode != "" {
		return strings.ToLower(record.ContinentCode), nil
	}
	if record.Continent != "" && len(record.Continent) == 2 {
		return strings.ToLower(record.Continent), nil
	}

	return "", errors.New("IP not found")
}
