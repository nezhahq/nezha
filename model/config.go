package model

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/go-viper/mapstructure/v2"
	kmaps "github.com/knadh/koanf/maps"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"sigs.k8s.io/yaml"

	"github.com/nezhahq/nezha/pkg/utils"
)

// JWTSecretEnvKey is the canonical environment variable that injects the JWT
// signing key. When set, the dashboard never writes the key to disk and the
// version-driven rotation in RotateJWTSecretKeyIfNeeded is skipped so that
// rotation is fully controlled by the operator / KMS.
const JWTSecretEnvKey = "NZ_JWTSECRETKEY" // #nosec G101 -- environment variable name, not a hardcoded secret value.

const (
	ConfigUsePeerIP                     = "NZ::Use-Peer-IP"
	JWTSecretKeyRotationBaselineVersion = "v2.0.13"
)

const (
	ConfigCoverAll = iota + 1
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

	// DashboardHost 是 dashboard 对外访问的主机名，专用于 OAuth2 回调地址。
	// 它与 InstallHost（agent 连接用主机名）解耦：两者可以是不同域名。
	// 为空时，OAuth2 回调放行请求 Host（信任请求头），不做强制重写。
	DashboardHost string `koanf:"dashboard_host" json:"dashboard_host,omitempty"`

	WebRealIPHeader   string `koanf:"web_real_ip_header" json:"web_real_ip_header,omitempty"`     // 前端真实IP
	AgentRealIPHeader string `koanf:"agent_real_ip_header" json:"agent_real_ip_header,omitempty"` // Agent真实IP
	UserTemplate      string `koanf:"user_template" json:"user_template,omitempty"`
	AdminTemplate     string `koanf:"admin_template" json:"admin_template,omitempty"`

	EnablePlainIPInNotification bool `koanf:"enable_plain_ip_in_notification" json:"enable_plain_ip_in_notification,omitempty"` // 通知信息IP不打码

	EnableMCP bool `koanf:"enable_mcp" json:"enable_mcp,omitempty"` // 是否启用 MCP 入口（默认关闭；启用前请审视 PAT scope/whitelist）

	// GHSA-x6fg-52vr-hj4w：反代部署下 dashboard 的对外域名进程自身看不到，
	// InstallHost/ListenHost 无法覆盖。运维在此用逗号分隔声明这些对外 host，
	// 成员便无法注册与之冲突的 NAT 域名抢占路由。
	ReservedHosts string `koanf:"reserved_hosts" json:"reserved_hosts,omitempty"`

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

	JWTSecretKey                   string `koanf:"jwt_secret_key" json:"-" yaml:"-"`
	JWTSecretKeyLastRotatedVersion string `koanf:"jwt_secret_key_last_rotated_version" json:"jwt_secret_key_last_rotated_version,omitempty"`
	ListenPort                     uint16 `koanf:"listen_port" json:"listen_port,omitempty"`
	ListenHost                     string `koanf:"listen_host" json:"listen_host,omitempty"`

	jwtSecretFromEnv  bool `koanf:"-" json:"-" yaml:"-"`
	jwtSecretFromYAML bool `koanf:"-" json:"-" yaml:"-"`

	// mcpEnabled：EnableMCP 的并发安全镜像，kill switch 跨 goroutine 读写走
	// MCPEnabled()/SetMCPEnabled()。放外层 Config 而非 ConfigDashboard，避免
	// SettingResponse 按值拷贝 ConfigDashboard 触发 copylocks。
	mcpEnabled atomic.Bool `koanf:"-" json:"-" yaml:"-"`

	// oauth2 配置
	Oauth2 map[string]*Oauth2Config `koanf:"oauth2" json:"oauth2,omitempty"`

	// HTTPS 配置
	HTTPS HTTPSConf `koanf:"https" json:"https"`

	// TSDB 配置
	TSDB TSDBConf `koanf:"tsdb" json:"tsdb"`

	// 内存配置
	Memory MemoryConf `koanf:"memory" json:"memory"`

	// 内置 LLM Chat 配置；放在 Config 顶层，不嵌入 ConfigDashboard，
	// 因此 listConfig 的游客 / 普通用户分支自然不会回显这些字段。
	// LLMAPIKey 用 json:"-" + yaml:"-"（参考 JWTSecretKey 模式），
	// 不参与 JSON / YAML 序列化；listConfig 时通过 LLMAPIKeySet 这个
	// 计算字段告诉前端"是否已配置"。
	EnableLLM       bool    `koanf:"enable_llm"        json:"enable_llm,omitempty"`
	LLMBaseURL      string  `koanf:"llm_base_url"      json:"llm_base_url,omitempty"`
	LLMModel        string  `koanf:"llm_model"         json:"llm_model,omitempty"`
	LLMAPIKey       string  `koanf:"llm_api_key"       json:"-"              yaml:"-"`
	LLMSystemPrompt string  `koanf:"llm_system_prompt" json:"llm_system_prompt,omitempty"`
	LLMMaxTokens    int     `koanf:"llm_max_tokens"    json:"llm_max_tokens,omitempty"`
	LLMTemperature  float32 `koanf:"llm_temperature"   json:"llm_temperature,omitempty"`

	// llmAPIKeySet：LLMAPIKey 是否非空的并发安全镜像，遵循 MCP 镜像字段的模式，
	// 避免 SettingResponse 整体值拷贝 Config 时读到撕裂的 LLMAPIKey。
	llmAPIKeySet atomic.Bool `koanf:"-" json:"-" yaml:"-"`

	k        *koanf.Koanf `json:"-"`
	filePath string       `json:"-"`
}

type HTTPSConf struct {
	InsecureTLS bool   `koanf:"insecure_tls" json:"insecure_tls,omitempty"`
	ListenPort  uint16 `koanf:"listen_port" json:"listen_port,omitempty"`
	TLSCertPath string `koanf:"tls_cert_path" json:"tls_cert_path,omitempty"`
	TLSKeyPath  string `koanf:"tls_key_path" json:"tls_key_path,omitempty"`
}

// TSDBConf TSDB 配置
type TSDBConf struct {
	DataPath                 string  `koanf:"data_path" json:"data_path,omitempty"`
	RetentionDays            uint16  `koanf:"retention_days" json:"retention_days,omitempty"`
	MinFreeDiskSpaceGB       float64 `koanf:"min_free_disk_space_gb" json:"min_free_disk_space_gb,omitempty"`
	MaxMemoryMB              int64   `koanf:"max_memory_mb" json:"max_memory_mb,omitempty"`
	WriteBufferSize          int     `koanf:"write_buffer_size" json:"write_buffer_size,omitempty"`
	WriteBufferFlushInterval int     `koanf:"write_buffer_flush_interval" json:"write_buffer_flush_interval,omitempty"`
}

// MemoryConf 内存配置
type MemoryConf struct {
	// GoMemLimitMB Go 运行时内存限制(MB)，0 表示不限制
	GoMemLimitMB int64 `koanf:"go_mem_limit_mb" json:"go_mem_limit_mb,omitempty"`
}

// Read 读取配置文件并应用
func (c *Config) Read(path string, frontendTemplates []FrontendTemplate) error {
	c.k = koanf.New(".")
	c.filePath = path

	err := c.k.Load(env.Provider("NZ_", ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, "NZ_")), "_", ".")
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
	if envSecret := os.Getenv(JWTSecretEnvKey); envSecret != "" {
		c.JWTSecretKey = envSecret
		c.jwtSecretFromEnv = true
	} else if c.JWTSecretKey != "" {
		c.jwtSecretFromYAML = true
		log.Printf("NEZHA>> jwt_secret_key loaded from config.yaml; recommend injecting via env %s to keep it off disk", JWTSecretEnvKey)
	}

	if c.JWTSecretKey == "" {
		generated, err := utils.GenerateRandomString(1024)
		if err != nil {
			return err
		}
		c.JWTSecretKey = generated
		c.jwtSecretFromYAML = true
		log.Printf("NEZHA>> generated new jwt_secret_key; wrote to config.yaml. For production, inject via env %s and remove the field from config.yaml.", JWTSecretEnvKey)
		if err := c.patchYAMLField("jwt_secret_key", generated); err != nil {
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

	c.mcpEnabled.Store(c.EnableMCP)
	c.llmAPIKeySet.Store(c.LLMAPIKey != "")

	return nil
}

// MCPEnabled 并发安全地读取 MCP kill switch 状态。
func (c *Config) MCPEnabled() bool {
	return c.mcpEnabled.Load()
}

// SetMCPEnabled 并发安全地更新 MCP kill switch 状态。只写 atomic 镜像，不直接
// 写 EnableMCP 明文字段——后者会与 listConfig 的 *singleton.Conf 整体拷贝读发生
// 数据竞争。持久化由 save() 在 marshal 前从 atomic 同步明文字段完成。
func (c *Config) SetMCPEnabled(v bool) {
	c.mcpEnabled.Store(v)
}

// LLMAPIKeySet 并发安全地读取 LLM API key 是否已配置。
func (c *Config) LLMAPIKeySet() bool {
	return c.llmAPIKeySet.Load()
}

// SetLLMAPIKey 并发安全地更新 LLM API key；保持 llmAPIKeySet 镜像同步。
// 调用方在写入失败时应回滚到 prev，避免明文与磁盘状态不一致。
func (c *Config) SetLLMAPIKey(key string) {
	c.LLMAPIKey = key
	c.llmAPIKeySet.Store(key != "")
}

// Save 保存配置文件
func (c *Config) Save() error {
	return c.save()
}

func (c *Config) RotateJWTSecretKeyIfNeeded(currentVersion string) (bool, error) {
	if c.jwtSecretFromEnv {
		return false, nil
	}

	currentVersion = strings.TrimSpace(currentVersion)
	if compareVersion(currentVersion, JWTSecretKeyRotationBaselineVersion) < 0 {
		return false, nil
	}

	initialMarker := c.JWTSecretKeyLastRotatedVersion
	shouldRotate := c.JWTSecretKeyLastRotatedVersion == "" || compareVersion(c.JWTSecretKeyLastRotatedVersion, JWTSecretKeyRotationBaselineVersion) < 0
	if shouldRotate {
		secret, err := utils.GenerateRandomString(1024)
		if err != nil {
			return false, err
		}
		c.JWTSecretKey = secret
	}

	c.JWTSecretKeyLastRotatedVersion = currentVersion

	if !shouldRotate && c.JWTSecretKeyLastRotatedVersion == initialMarker {
		return false, nil
	}
	if shouldRotate {
		if err := c.patchYAMLField("jwt_secret_key", c.JWTSecretKey); err != nil {
			return false, err
		}
	}
	if err := c.patchYAMLField("jwt_secret_key_last_rotated_version", c.JWTSecretKeyLastRotatedVersion); err != nil {
		return false, err
	}
	return shouldRotate, nil
}

func (c *Config) patchYAMLField(key string, value any) error {
	return c.PatchYAMLField(key, value)
}

// PatchYAMLField 是 patchYAMLField 的导出版本，供 settings 控制器等需要
// 增量更新单个 YAML 键的代码使用；与 save() 的全量 marshal 不同，它只改一个
// 键，保留文件里其它未映射到 struct 的字段。
func (c *Config) PatchYAMLField(key string, value any) error {
	dir := filepath.Dir(c.filePath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}

	raw := map[string]any{}
	if data, err := os.ReadFile(c.filePath); err == nil {
		if len(data) > 0 {
			if err := yaml.Unmarshal(data, &raw); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	raw[key] = value

	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(c.filePath, out, 0600)
}

func (c *Config) save() error {
	c.EnableMCP = c.mcpEnabled.Load()
	// LLMAPIKey 不进入 YAML（json:"-" yaml:"-"），无需同步明文字段；
	// llmAPIKeySet 镜像同步在调用 SetLLMAPIKey 时已完成。
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

func compareVersion(left, right string) int {
	leftParts, leftOK := parseVersion(left)
	rightParts, rightOK := parseVersion(right)
	if !leftOK || !rightOK {
		return -1
	}
	for i := range leftParts {
		if leftParts[i] < rightParts[i] {
			return -1
		}
		if leftParts[i] > rightParts[i] {
			return 1
		}
	}
	return 0
}

func parseVersion(version string) ([3]int, bool) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var parsed [3]int
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return [3]int{}, false
		}
		parsed[i] = value
	}
	return parsed, true
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
