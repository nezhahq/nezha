package controller

import (
	"errors"
	"log"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
)

// List settings
// @Summary List settings
// @Schemes
// @Description List settings
// @Security BearerAuth
// @Tags common
// @Produce json
// @Success 200 {object} model.CommonResponse[model.SettingResponse]
// @Router /setting [get]
func listConfig(c *gin.Context) (*model.SettingResponse, error) {
	u, authorized := c.Get(model.CtxKeyAuthorizedUser)
	var isAdmin bool
	if authorized {
		user := u.(*model.User)
		isAdmin = user.Role.IsAdmin()
	}

	config := *singleton.Conf
	config.Language = strings.ReplaceAll(config.Language, "_", "-")

	conf := model.SettingResponse{
		Config: model.Setting{
			ConfigForGuests:                config.ConfigForGuests,
			ConfigDashboard:                config.ConfigDashboard,
			IgnoredIPNotificationServerIDs: config.IgnoredIPNotificationServerIDs,
			Oauth2Providers:                config.Oauth2Providers,
			// LLM 字段：仅管理员分支写入；LLMAPIKeySet 是计算字段（mirror），
			// LLMAPIKey 明文永不回显。
			EnableLLM:       config.EnableLLM,
			LLMBaseURL:      config.LLMBaseURL,
			LLMModel:        config.LLMModel,
			LLMAPIKeySet:    config.LLMAPIKeySet(),
			LLMSystemPrompt: config.LLMSystemPrompt,
			LLMMaxTokens:    config.LLMMaxTokens,
			LLMTemperature:  config.LLMTemperature,
		},
		Version:           singleton.Version,
		FrontendTemplates: singleton.FrontendTemplates,
		TSDBEnabled:       singleton.TSDBEnabled(),
	}

	if !authorized || !isAdmin {
		configForGuests := config.ConfigForGuests
		var configDashboard model.ConfigDashboard
		if authorized {
			configDashboard.AgentTLS = singleton.Conf.AgentTLS
			configDashboard.InstallHost = singleton.Conf.InstallHost
		}
		conf = model.SettingResponse{
			Config: model.Setting{
				ConfigForGuests: configForGuests,
				ConfigDashboard: configDashboard,
				Oauth2Providers: config.Oauth2Providers,
			},
			TSDBEnabled: singleton.TSDBEnabled(),
		}
	}

	return &conf, nil
}

// Edit config
// @Summary Edit config
// @Security BearerAuth
// @Schemes
// @Description Edit config
// @Tags admin required
// @Accept json
// @Param body body model.SettingForm true "SettingForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /setting [patch]
func updateConfig(c *gin.Context) (any, error) {
	var sf model.SettingForm
	if err := c.ShouldBindJSON(&sf); err != nil {
		return nil, err
	}
	var userTemplateValid bool
	for _, v := range singleton.FrontendTemplates {
		if !userTemplateValid && v.Path == sf.UserTemplate && !v.IsAdmin {
			userTemplateValid = true
		}
		if userTemplateValid {
			break
		}
	}
	if !userTemplateValid {
		return nil, errors.New("invalid user template")
	}

	singleton.Conf.Language = strings.ReplaceAll(sf.Language, "-", "_")

	singleton.Conf.EnableIPChangeNotification = sf.EnableIPChangeNotification
	singleton.Conf.EnablePlainIPInNotification = sf.EnablePlainIPInNotification
	singleton.Conf.Cover = sf.Cover
	singleton.Conf.InstallHost = sf.InstallHost
	singleton.Conf.DashboardHost = sf.DashboardHost
	singleton.Conf.ReservedHosts = sf.ReservedHosts
	singleton.Conf.IgnoredIPNotification = sf.IgnoredIPNotification
	singleton.Conf.IPChangeNotificationGroupID = sf.IPChangeNotificationGroupID
	singleton.Conf.SiteName = sf.SiteName
	singleton.Conf.DNSServers = sf.DNSServers
	singleton.Conf.CustomCode = sf.CustomCode
	singleton.Conf.CustomCodeDashboard = sf.CustomCodeDashboard
	singleton.Conf.WebRealIPHeader = sf.WebRealIPHeader
	singleton.Conf.AgentRealIPHeader = sf.AgentRealIPHeader
	singleton.Conf.AgentTLS = sf.AgentTLS
	singleton.Conf.UserTemplate = sf.UserTemplate
	mcpWasEnabled := singleton.Conf.MCPEnabled()
	mcpNext := resolveSettingEnableMCP(sf.EnableMCP, mcpWasEnabled)

	if err := applyEnableMCPTransition(
		mcpWasEnabled, mcpNext,
		singleton.Conf.SetMCPEnabled,
		singleton.Conf.Save,
		fireMCPKillSwitch,
	); err != nil {
		return nil, newGormError("%v", err)
	}

	if err := applyLLMTransition(&sf); err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.OnUpdateLang(singleton.Conf.Language)
	return nil, nil
}

// applyLLMTransition 把 SettingForm 中的 LLM 字段写到 singleton.Conf 并落盘，
// 维持以下不变量：
//   - LLMAPIKey 用 *string 实现 PATCH 语义：nil 表示"未传"，保留磁盘原值；
//     空串表示"显式清空"。明文不入 YAML 也入不了 JSON，所以写入走 patchYAMLField
//     而非 Save()，避免把其它未改动字段冲掉。
//   - 写盘失败时整体回滚 in-memory 字段到 prev（与 applyEnableMCPTransition
//     同款），不让面板处于半配置状态。
//   - 字段真正落盘成功后调用 LLMShared.Reload()，让客户端按最新配置重建。
func applyLLMTransition(sf *model.SettingForm) error {
	prevKey := singleton.Conf.LLMAPIKey

	// 写非敏感字段到 in-memory；这些字段随后会被 Save() 序列化进 YAML。
	if sf.EnableLLM != nil {
		singleton.Conf.EnableLLM = *sf.EnableLLM
	}
	if sf.LLMBaseURL != nil {
		singleton.Conf.LLMBaseURL = strings.TrimSpace(*sf.LLMBaseURL)
	}
	if sf.LLMModel != nil {
		singleton.Conf.LLMModel = strings.TrimSpace(*sf.LLMModel)
	}
	if sf.LLMSystemPrompt != nil {
		singleton.Conf.LLMSystemPrompt = *sf.LLMSystemPrompt
	}
	if sf.LLMMaxTokens != nil && *sf.LLMMaxTokens >= 0 {
		singleton.Conf.LLMMaxTokens = *sf.LLMMaxTokens
	}
	if sf.LLMTemperature != nil && *sf.LLMTemperature >= 0 {
		singleton.Conf.LLMTemperature = *sf.LLMTemperature
	}

	if err := singleton.Conf.Save(); err != nil {
		return err
	}

	// API key 走独立事务：仅在用户传了字段时落盘，避免 Save 后被 yaml.Marshal
	// (LLMAPIKey 是 yaml:"-") 漏写。
	// nil   = 字段未传 → 不修改
	// ""    = 显式空 → 也视为"不修改"（防御性，前端应剔除空字符串；清空请直接编辑
	//          data/config.yaml，避免 UI 误操作把生产 key 清掉）
	// 非空  = 覆盖
	if sf.LLMAPIKey != nil && *sf.LLMAPIKey != "" {
		singleton.Conf.SetLLMAPIKey(*sf.LLMAPIKey)
		if err := singleton.Conf.PatchYAMLField("llm_api_key", *sf.LLMAPIKey); err != nil {
			// 写盘失败：API key 已经在内存里设了，回滚到 prev。
			singleton.Conf.SetLLMAPIKey(prevKey)
			return err
		}
	}

	// 任一字段真正落盘成功后才触发 Reload，避免半配置状态下的客户端被重建。
	if singleton.LLMShared != nil {
		if err := singleton.LLMShared.Reload(); err != nil {
			log.Printf("NEZHA>> LLM reload after settings update failed: %v", err)
		}
	}
	return nil
}

// applyEnableMCPTransition commits the new EnableMCP value and persists it,
// guaranteeing the in-memory flag and the kill-switch cleanup stay consistent
// with what actually reached durable storage:
//   - setVal(next) is applied so Save serialises the new value.
//   - If save fails, the flag is rolled back to prev and no cleanup runs, so a
//     failed disable cannot leave the dashboard half-disabled (new requests
//     rejected while in-flight RPC/streams/URLs are never revoked).
//   - cleanup runs only on a persisted enabled->disabled transition.
func applyEnableMCPTransition(prev, next bool, setVal func(bool), save func() error, cleanup func()) error {
	setVal(next)
	if err := save(); err != nil {
		setVal(prev)
		return err
	}
	if prev && !next {
		cleanup()
	}
	return nil
}

func fireMCPKillSwitch() {
	purgedURLs := PurgeTransferEntries()
	revokedStreams := rpc.NezhaHandlerSingleton.RevokeStreamsForPurpose(rpc.PurposeMCPTransfer)
	cancelledRPC := rpc.CancelAllMCPInflight()
	log.Printf("NEZHA>> MCP kill switch fired: purged=%d urls, revoked=%d streams, cancelled=%d rpc",
		purgedURLs, revokedStreams, cancelledRPC)
}

// resolveSettingEnableMCP picks the effective EnableMCP value for the
// update. A nil form pointer means "field absent" so we MUST preserve
// the current config to avoid accidentally tripping the kill switch on
// partial PATCH calls that omit enable_mcp.
func resolveSettingEnableMCP(formValue *bool, current bool) bool {
	if formValue == nil {
		return current
	}
	return *formValue
}

// Perform maintenance
// @Summary Perform maintenance
// @Security BearerAuth
// @Schemes
// @Description Perform system maintenance (SQLite VACUUM and TSDB maintenance)
// @Tags admin required
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /maintenance [post]
func runMaintenance(c *gin.Context) (any, error) {
	singleton.PerformMaintenance()
	return nil, nil
}
