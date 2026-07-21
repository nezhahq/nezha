package model

type SettingForm struct {
	DNSServers                  string `json:"dns_servers,omitempty" validate:"optional"`
	IgnoredIPNotification       string `json:"ignored_ip_notification,omitempty" validate:"optional"`
	IPChangeNotificationGroupID uint64 `json:"ip_change_notification_group_id,omitempty"` // IP变更提醒的通知组
	Cover                       uint8  `json:"cover,omitempty"`
	SiteName                    string `json:"site_name,omitempty" minLength:"1"`
	Language                    string `json:"language,omitempty" minLength:"2"`
	InstallHost                 string `json:"install_host,omitempty" validate:"optional"`
	DashboardHost               string `json:"dashboard_host,omitempty" validate:"optional"`
	ReservedHosts               string `json:"reserved_hosts,omitempty" validate:"optional"`
	CustomCode                  string `json:"custom_code,omitempty" validate:"optional"`
	CustomCodeDashboard         string `json:"custom_code_dashboard,omitempty" validate:"optional"`
	WebRealIPHeader             string `json:"web_real_ip_header,omitempty" validate:"optional"`   // 前端真实IP
	AgentRealIPHeader           string `json:"agent_real_ip_header,omitempty" validate:"optional"` // Agent真实IP
	UserTemplate                string `json:"user_template,omitempty" validate:"optional"`

	AgentTLS                    bool  `json:"tls,omitempty" validate:"optional"`
	EnableIPChangeNotification  bool  `json:"enable_ip_change_notification,omitempty" validate:"optional"`
	EnablePlainIPInNotification bool  `json:"enable_plain_ip_in_notification,omitempty" validate:"optional"`
	EnableMCP                   *bool `json:"enable_mcp,omitempty" validate:"optional"`

	// LLM Chat 配置。
	// LLMAPIKey 用 *string：nil = 未传（保留磁盘原值），"" = 显式清空。
	// 与 EnableMCP *bool 的 PATCH 语义一致。
	EnableLLM       *bool    `json:"enable_llm,omitempty"        validate:"optional"`
	LLMBaseURL      *string  `json:"llm_base_url,omitempty"      validate:"optional"`
	LLMModel        *string  `json:"llm_model,omitempty"         validate:"optional"`
	LLMAPIKey       *string  `json:"llm_api_key,omitempty"       validate:"optional"`
	LLMSystemPrompt *string  `json:"llm_system_prompt,omitempty" validate:"optional"`
	LLMMaxTokens    *int     `json:"llm_max_tokens,omitempty"    validate:"optional"`
	LLMTemperature  *float32 `json:"llm_temperature,omitempty"   validate:"optional"`
}

type Setting struct {
	ConfigForGuests
	ConfigDashboard

	IgnoredIPNotificationServerIDs map[uint64]bool `json:"ignored_ip_notification_server_ids,omitempty"`
	Oauth2Providers                []string        `json:"oauth2_providers,omitempty"`

	// LLM Chat 配置（仅管理员回显）。LLMAPIKeySet 是计算字段：
	// 由 Config.LLMAPIKeySet() 提供；listConfig 把其镜像拷到此处以便
	// 前端据此显示"已配置 / 未配置"，但 LLMAPIKey 明文永不回显。
	EnableLLM       bool    `json:"enable_llm,omitempty"`
	LLMBaseURL      string  `json:"llm_base_url,omitempty"`
	LLMModel        string  `json:"llm_model,omitempty"`
	LLMAPIKeySet    bool    `json:"llm_api_key_set,omitempty"`
	LLMSystemPrompt string  `json:"llm_system_prompt,omitempty"`
	LLMMaxTokens    int     `json:"llm_max_tokens,omitempty"`
	LLMTemperature  float32 `json:"llm_temperature,omitempty"`
}

type FrontendTemplate struct {
	Path       string `json:"path,omitempty"`
	Name       string `json:"name,omitempty"`
	Repository string `json:"repository,omitempty"`
	Author     string `json:"author,omitempty"`
	Version    string `json:"version,omitempty"`
	IsAdmin    bool   `json:"is_admin,omitempty"`
	IsOfficial bool   `json:"is_official,omitempty"`
}

type SettingResponse struct {
	Config Setting `json:"config"`

	Version           string             `json:"version,omitempty"`
	FrontendTemplates []FrontendTemplate `json:"frontend_templates,omitempty"`
	TSDBEnabled       bool               `json:"tsdb_enabled"`
}
