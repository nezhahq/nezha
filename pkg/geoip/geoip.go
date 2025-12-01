package geoip

import (
	_ "embed"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	maxminddb "github.com/oschwald/maxminddb-golang"
)

//====================
// 1. 内置 geoip.db
//====================

//go:embed geoip.db
var embeddedDB []byte

//====================
// 2. 外部 ipinfo_lite.mmdb
//====================

// 容器内部的路径：对应宿主机 /opt/nezha/dashboard/data/ipinfo_lite.mmdb
const externalDBPath = "/dashboard/data/ipinfo_lite.mmdb"

var (
	dbOnce    sync.Once
	dbReader  *maxminddb.Reader
	dbInitErr error
)

func initDB() {
	// 优先尝试外部 ipinfo_lite.mmdb
	if info, err := os.Stat(externalDBPath); err == nil && !info.IsDir() {
		if reader, err := maxminddb.Open(externalDBPath); err == nil {
			dbReader = reader
			return
		}
		// 如果打开失败，就继续往下，用内置的 embeddedDB
	}

	// 外部文件不存在或失败 → 回退到内置 geoip.db
	reader, err := maxminddb.FromBytes(embeddedDB)
	if err != nil {
		dbInitErr = err
		return
	}
	dbReader = reader
}

func getDB() (*maxminddb.Reader, error) {
	dbOnce.Do(initDB)
	return dbReader, dbInitErr
}

// 支持两种 mmdb 格式：
// - 内置 geoip.db：country/continent 是代码，country_name/continent_name 是名字
// - 外部 ipinfo_lite.mmdb：country/country_name 是名字，country_code/continent_code 是代码
type IPInfo struct {
	CountryCode   string `maxminddb:"country_code"`
	Country       string `maxminddb:"country"`
	CountryName   string `maxminddb:"country_name"`
	ContinentCode string `maxminddb:"continent_code"`
	Continent     string `maxminddb:"continent"`
	ContinentName string `maxminddb:"continent_name"`
}

//====================
// 3. 先用 ipinfo.io 查询
//====================

var httpClient = &http.Client{
	Timeout: 2 * time.Second,
}

// 通用的请求+解析函数：请求 url，返回 2 位小写国家码
func fetchIPInfoCountry(url string) (string, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
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

	// 统一转成小写返回
	return strings.ToLower(code), nil
}

// 尝试从 ipinfo 获取国家代码；如果没有 TOKEN，直接视为失败
// token 从环境变量 IPINFO_TOKEN 读取；为空则跳过 ipinfo 调用
// token 位置dashboard\docker-compose.yaml
func lookupFromIPInfo(ip net.IP) (string, error) {
	if ip == nil {
		return "", errors.New("nil ip")
	}

	token := os.Getenv("IPINFO_TOKEN")
	if token == "" {
		// 没有设置 token，跳过 ipinfo，交给后续 mmdb 逻辑处理
		return "", errors.New("ipinfo token not set")
	}

	url := "https://ipinfo.io/" + ip.String() + "/country?token=" + token
	return fetchIPInfoCountry(url)
}

//====================
// 4. 对外暴露的 Lookup
//====================

func Lookup(ip net.IP) (string, error) {
	// 1) 优先用 ipinfo.io（先 token，再无 token）
	if code, err := lookupFromIPInfo(ip); err == nil && code != "" {
		// code 已经是 2 位小写，如 hk、us、cn
		return code, nil
	}

	// 2) 外部调用失败 → 回退到 mmdb 逻辑
	db, err := getDB()
	if err != nil {
		return "", err
	}

	var record IPInfo
	if err := db.Lookup(ip, &record); err != nil {
		return "", err
	}

	// ==== 国家码优先级 ====
	// 1) 优先 country_code（适配你外部化的 mmdb）
	if record.CountryCode != "" {
		return strings.ToLower(record.CountryCode), nil
	}
	// 2) 其次 country 刚好是 2 位（适配内置 mmdb 把代码放在 country 的情况）
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
