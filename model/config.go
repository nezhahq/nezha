package model

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	kyaml "github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"gopkg.in/yaml.v3"

	"github.com/nezhahq/nezha/pkg/utils"
)

const (
	ConfigUsePeerIP = "NZ::Use-Peer-IP"
	ConfigCoverAll  = iota
	ConfigCoverIgnoreAll
)

type ConfigForGuests struct {
	Language            string   `json:"language"`
	SiteName            string   `json:"site_name"`
	CustomCode          string   `json:"custom_code,omitempty"`
	CustomCodeDashboard string   `json:"custom_code_dashboard,omitempty"`
	Oauth2Providers     []string `json:"oauth2_providers,omitempty"`

	InstallHost string `json:"install_host,omitempty"`
	TLS         bool   `json:"tls,omitempty"`
}

type Config struct {
	Debug        bool   `mapstructure:"debug" json:"debug,omitempty"`                   // debug模式开关
	RealIPHeader string `mapstructure:"real_ip_header" json:"real_ip_header,omitempty"` // 真实IP

	Language       string `mapstructure:"language" json:"language"` // 系统语言，默认 zh_CN
	SiteName       string `mapstructure:"site_name" json:"site_name"`
	UserTemplate   string `mapstructure:"user_template" json:"user_template,omitempty"`
	AdminTemplate  string `mapstructure:"admin_template" json:"admin_template,omitempty"`
	JWTSecretKey   string `mapstructure:"jwt_secret_key" json:"jwt_secret_key,omitempty"`
	AgentSecretKey string `mapstructure:"agent_secret_key" json:"agent_secret_key,omitempty"`
	ListenPort     uint   `mapstructure:"listen_port" json:"listen_port,omitempty"`
	ListenHost     string `mapstructure:"listen_host" json:"listen_host,omitempty"`
	InstallHost    string `mapstructure:"install_host" json:"install_host,omitempty"`
	TLS            bool   `mapstructure:"tls" json:"tls,omitempty"`
	Location       string `mapstructure:"location" json:"location,omitempty"`     // 时区，默认为 Asia/Shanghai
	ForceAuth      bool   `mapstructure:"force_auth" json:"force_auth,omitempty"` // 强制要求认证

	EnablePlainIPInNotification bool `mapstructure:"enable_plain_ip_in_notification" json:"enable_plain_ip_in_notification,omitempty"` // 通知信息IP不打码

	// IP变更提醒
	EnableIPChangeNotification  bool   `mapstructure:"enable_ip_change_notification" json:"enable_ip_change_notification,omitempty"`
	IPChangeNotificationGroupID uint64 `mapstructure:"ip_change_notification_group_id" json:"ip_change_notification_group_id"`
	Cover                       uint8  `mapstructure:"cover" json:"cover"`                                               // 覆盖范围（0:提醒未被 IgnoredIPNotification 包含的所有服务器; 1:仅提醒被 IgnoredIPNotification 包含的服务器;）
	IgnoredIPNotification       string `mapstructure:"ignored_ip_notification" json:"ignored_ip_notification,omitempty"` // 特定服务器IP（多个服务器用逗号分隔）

	IgnoredIPNotificationServerIDs map[uint64]bool `mapstructure:"ignored_ip_notification_server_ids" json:"ignored_ip_notification_server_ids,omitempty"` // [ServerID] -> bool(值为true代表当前ServerID在特定服务器列表内）
	AvgPingCount                   int             `mapstructure:"avg_ping_count" json:"avg_ping_count,omitempty"`
	DNSServers                     string          `mapstructure:"dns_servers" json:"dns_servers,omitempty"`

	CustomCode          string `mapstructure:"custom_code" json:"custom_code,omitempty"`
	CustomCodeDashboard string `mapstructure:"custom_code_dashboard" json:"custom_code_dashboard,omitempty"`

	// oauth2 配置
	Oauth2 map[string]*Oauth2Config `mapstructure:"oauth2" json:"oauth2,omitempty"`
	// oauth2 供应商列表，无需配置，自动生成
	Oauth2Providers []string `yaml:"-" json:"oauth2_providers,omitempty"`

	k        *koanf.Koanf `json:"-"`
	filePath string       `json:"-"`
}

// Read 读取配置文件并应用
func (c *Config) Read(path string, frontendTemplates []FrontendTemplate) error {
	c.k = koanf.New(".")
	c.filePath = path

	err := c.k.Load(env.Provider("NZ_", ".", func(s string) string {
		return strings.Replace(strings.ToLower(strings.TrimPrefix(s, "NZ_")), "_", ".", -1)
	}), nil)
	if err != nil {
		return err
	}

	if _, err := os.Stat(path); err == nil {
		err = c.k.Load(file.Provider(path), kyaml.Parser())
		if err != nil {
			return err
		}
	}

	err = c.k.Unmarshal("", c)
	if err != nil {
		return err
	}
	if c.ListenPort == 0 {
		c.ListenPort = 8008
	}
	if c.Language == "" {
		c.Language = "en_US"
	}
	if c.Location == "" {
		c.Location = "Asia/Shanghai"
	}
	var userTemplateValid, adminTemplateValid bool
	for _, v := range frontendTemplates {
		if !userTemplateValid && v.Path == c.UserTemplate && !v.IsAdmin {
			userTemplateValid = true
		}
		if !adminTemplateValid && v.Path == c.AdminTemplate && v.IsAdmin {
			adminTemplateValid = true
		}
		if userTemplateValid && adminTemplateValid {
			break
		}
	}
	if c.UserTemplate == "" || !userTemplateValid {
		c.UserTemplate = "user-dist"
	}
	if c.AdminTemplate == "" || !adminTemplateValid {
		c.AdminTemplate = "admin-dist"
	}
	if c.AvgPingCount == 0 {
		c.AvgPingCount = 2
	}
	if c.Cover == 0 {
		c.Cover = 1
	}
	if c.JWTSecretKey == "" {
		c.JWTSecretKey, err = utils.GenerateRandomString(1024)
		if err != nil {
			return err
		}
		if err = c.Save(); err != nil {
			return err
		}
	}

	if c.AgentSecretKey == "" {
		c.AgentSecretKey, err = utils.GenerateRandomString(32)
		if err != nil {
			return err
		}
		if err = c.Save(); err != nil {
			return err
		}
	}

	c.Oauth2Providers = slices.Collect(maps.Keys(c.Oauth2))

	c.updateIgnoredIPNotificationID()
	return nil
}

// updateIgnoredIPNotificationID 更新用于判断服务器ID是否属于特定服务器的map
func (c *Config) updateIgnoredIPNotificationID() {
	c.IgnoredIPNotificationServerIDs = make(map[uint64]bool)
	splitedIDs := strings.Split(c.IgnoredIPNotification, ",")
	for i := 0; i < len(splitedIDs); i++ {
		id, _ := strconv.ParseUint(splitedIDs[i], 10, 64)
		if id > 0 {
			c.IgnoredIPNotificationServerIDs[id] = true
		}
	}
}

// Save 保存配置文件
func (c *Config) Save() error {
	c.updateIgnoredIPNotificationID()
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}

	dir := filepath.Dir(c.filePath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}

	return os.WriteFile(c.filePath, data, 0600)
}
