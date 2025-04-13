package model

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	kmaps "github.com/knadh/koanf/maps"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"sigs.k8s.io/yaml"

	"github.com/nezhahq/nezha/pkg/utils"
)

const (
	ConfigUsePeerIP = "NZ::Use-Peer-IP"
	ConfigCoverAll  = iota
	ConfigCoverIgnoreAll
)

type ConfigForGuests struct {
	Language            string `koanf:"language" json:"language"` // 系统语言，默认 zh_CN
	SiteName            string `koanf:"site_name" json:"site_name"`
	CustomCode          string `koanf:"custom_code" json:"custom_code,omitempty"`
	CustomCodeDashboard string `koanf:"custom_code_dashboard" json:"custom_code_dashboard,omitempty"`
}

type ConfigDashboard struct {
	InstallHost string `koanf:"install_host" json:"install_host,omitempty"`
	AgentTLS    bool   `koanf:"tls" json:"tls,omitempty"` // 用于前端判断生成的安装命令是否启用 TLS

	WebRealIPHeader  string `koanf:"web_real_ip_header" json:"web_real_ip_header,omitempty"` // 前端真实IP
	AgentRealIPHeader  string `koanf:"agent_real_ip_header" json:"agent_real_ip_header,omitempty"` // Agent真实IP
	UserTemplate  string `koanf:"user_template" json:"user_template,omitempty"`
	AdminTemplate string `koanf:"admin_template" json:"admin_template,omitempty"`

	EnablePlainIPInNotification bool `koanf:"enable_plain_ip_in_notification" json:"enable_plain_ip_in_notification,omitempty"` // 通知信息IP不打码

	// IP变更提醒
	EnableIPChangeNotification  bool   `koanf:"enable_ip_change_notification" json:"enable_ip_change_notification,omitempty"`
	IPChangeNotificationGroupID uint64 `koanf:"ip_change_notification_group_id" json:"ip_change_notification_group_id"`
	Cover                       uint8  `koanf:"cover" json:"cover"`                                               // 覆盖范围（0:提醒未被 IgnoredIPNotification 包含的所有服务器; 1:仅提醒被 IgnoredIPNotification 包含的服务器;）
	IgnoredIPNotification       string `koanf:"ignored_ip_notification" json:"ignored_ip_notification,omitempty"` // 特定服务器IP（多个服务器用逗号分隔）

	DNSServers string `koanf:"dns_servers" json:"dns_servers,omitempty"`
}

type Config struct {
	ConfigForGuests
	ConfigDashboard

	AvgPingCount int `koanf:"avg_ping_count" json:"avg_ping_count,omitempty"`

	Debug          bool   `koanf:"debug" json:"debug,omitempty"`           // debug模式开关
	Location       string `koanf:"location" json:"location,omitempty"`     // 时区，默认为 Asia/Shanghai
	ForceAuth      bool   `koanf:"force_auth" json:"force_auth,omitempty"` // 强制要求认证
	AgentSecretKey string `koanf:"agent_secret_key" json:"agent_secret_key,omitempty"`
	JWTTimeout     int    `koanf:"jwt_timeout" json:"jwt_timeout,omitempty"` // JWT token过期时间（小时）

	JWTSecretKey string `koanf:"jwt_secret_key" json:"jwt_secret_key,omitempty"`
	ListenPort   uint16 `koanf:"listen_port" json:"listen_port,omitempty"`
	ListenHost   string `koanf:"listen_host" json:"listen_host,omitempty"`

	// oauth2 配置
	Oauth2 map[string]*Oauth2Config `koanf:"oauth2" json:"oauth2,omitempty"`

	// HTTPS 配置
	HTTPS HTTPSConf `koanf:"https" json:"https"`

	k        *koanf.Koanf `json:"-"`
	filePath string       `json:"-"`
}

type HTTPSConf struct {
	InsecureTLS bool   `koanf:"insecure_tls" json:"insecure_tls,omitempty"`
	ListenPort  uint16 `koanf:"listen_port" json:"listen_port,omitempty"`
	TLSCertPath string `koanf:"tls_cert_path" json:"tls_cert_path,omitempty"`
	TLSKeyPath  string `koanf:"tls_key_path" json:"tls_key_path,omitempty"`
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
		err = c.k.Load(file.Provider(path), new(utils.KubeYAML), koanf.WithMergeFunc(mergeDedup))
		if err != nil {
			return err
		}
	}

	err = c.k.UnmarshalWithConf("", c, koanfConf(c))
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

	// Add JWTTimeout default check
	if c.JWTTimeout == 0 {
		c.JWTTimeout = 1
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

	return nil
}

// Save 保存配置文件
func (c *Config) Save() error {
	return c.save()
}

func (c *Config) save() error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}

	return c.write(data)
}

func (c *Config) write(data []byte) error {
	dir := filepath.Dir(c.filePath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}

	return os.WriteFile(c.filePath, data, 0600)
}

func koanfConf(c any) koanf.UnmarshalConf {
	return koanf.UnmarshalConf{
		DecoderConfig: &mapstructure.DecoderConfig{
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToTimeDurationHookFunc(),
				utils.TextUnmarshalerHookFunc()),
			Metadata:         nil,
			Result:           c,
			WeaklyTypedInput: true,
			MatchName: func(mapKey, fieldName string) bool {
				return strings.EqualFold(mapKey, fieldName) ||
					strings.EqualFold(mapKey, strings.ReplaceAll(fieldName, "_", ""))
			},
			Squash: true,
		},
	}
}

func mergeDedup(src, dst map[string]any) error {
	for key := range src {
		if strings.IndexByte(key, '_') == -1 {
			continue
		}

		oldKey := strings.ReplaceAll(key, "_", "")
		if _, ok := dst[oldKey]; ok {
			src[oldKey] = src[key]
			delete(src, key)
		}
	}

	kmaps.Merge(src, dst)
	return nil
}
