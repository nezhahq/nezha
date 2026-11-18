package controller

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/idna"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// 写入侧的资源引用规范化与上限（GHSA-8fgr-wx8p-v6q7）。
//
// 低权限成员曾可以用以下方式把落库 JSON 撑到任意大小：
//   - 值为 false 的 skip_servers / rule.ignore 条目（权限校验只看 true）
//   - 重复的 ddns_profiles / trigger task / cron servers ID（存在性校验不去重）
//   - 不存在的 server / cron ID（通用 class.CheckPermission 跳过缺失 ID）
//   - 完全未校验的 override_ddns_domains、无数量上限的 domains / rules
//
// 规范化必须发生在权限校验之前，后续校验与落库都基于规范化后的集合，
// 这样集合大小被真实对象数量约束，而不是被请求体大小约束。

const (
	// restAPIRequestMaxBodyBytes 是 /api/v1 下所有请求体的上限。最大的合法
	// 请求是管理员的自定义代码设置，远小于该值。
	restAPIRequestMaxBodyBytes = 1 << 20
	// maxDomainsPerDDNSProfile 同时约束 DDNS profile 的 domains 与 server 的
	// 单个 profile 覆盖域名列表；每个域名都会触发一次外部 DNS 更新。
	maxDomainsPerDDNSProfile = 256
	// maxRulesPerAlertRule 约束单条告警的子规则数；每个上报样本都会为每条
	// 子规则保存一份数据并逐条求值。
	maxRulesPerAlertRule = 64
)

// restRequestBodyLimitMiddleware 在 handler 解析请求体前装上 MaxBytesReader。
// 必须挂在路由组上：gin 的 ShouldBindJSON 会完整读取请求体。
func restRequestBodyLimitMiddleware(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil && c.Request.Body != http.NoBody {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
		c.Next()
	}
}

// existingServerIDs 去重并丢弃不存在的 server。仅用于 allow-set 语义的集合：
// 不存在的 server 不会被调度，丢弃它们不改变行为。deny-set 语义的集合由
// denyListOwnedByCaller 严格要求存在，不走这里。
func existingServerIDs(ids []uint64) []uint64 {
	ids = model.UniqueIDs(ids)
	if ids == nil {
		return nil
	}
	existing := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if s, ok := singleton.ServerShared.Get(id); ok && s != nil {
			existing = append(existing, id)
		}
	}
	return existing
}

func existingServerIDSet(set map[uint64]bool) map[uint64]bool {
	enabled := model.EnabledIDSet(set)
	for id := range enabled {
		if s, ok := singleton.ServerShared.Get(id); !ok || s == nil {
			delete(enabled, id)
		}
	}
	return enabled
}

// existingCronIDs 去重并丢弃不存在的 cron：SendTriggerTasks 本就跳过缺失 ID。
func existingCronIDs(ids []uint64) []uint64 {
	ids = model.UniqueIDs(ids)
	if ids == nil {
		return nil
	}
	existing := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if cr, ok := singleton.CronShared.Get(id); ok && cr != nil {
			existing = append(existing, id)
		}
	}
	return existing
}

// normalizeDDNSDomains 做 IDN→ASCII、去重并限制数量。
func normalizeDDNSDomains(domains []string) ([]string, error) {
	if len(domains) > maxDomainsPerDDNSProfile {
		return nil, singleton.Localizer.ErrorT("too many domains, at most %d", maxDomainsPerDDNSProfile)
	}
	if domains == nil {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(domains))
	normalized := make([]string, 0, len(domains))
	for _, domain := range domains {
		ascii, err := idna.Lookup.ToASCII(domain)
		if err != nil {
			return nil, singleton.Localizer.ErrorT("error parsing %s: %v", domain, err)
		}
		if _, ok := seen[ascii]; ok {
			continue
		}
		seen[ascii] = struct{}{}
		normalized = append(normalized, ascii)
	}
	return normalized, nil
}

// normalizeOverrideDDNSDomains 只保留已绑定 profile 的覆盖域名，并按 profile
// 域名同样的规则校验。此前该字段完全未校验，绕过了 createDDNS 的 IDN 校验。
func normalizeOverrideDDNSDomains(overrides map[uint64][]string, profiles []uint64) (map[uint64][]string, error) {
	pruned := model.PruneOverrideDDNSDomains(overrides, profiles)
	for id, domains := range pruned {
		normalized, err := normalizeDDNSDomains(domains)
		if err != nil {
			return nil, err
		}
		pruned[id] = normalized
	}
	return pruned, nil
}

func normalizeServiceReferences(m *model.Service) {
	if m.Cover == model.ServiceCoverIgnoreAll {
		m.SkipServers = existingServerIDSet(m.SkipServers)
	} else {
		// CoverAll 的 deny-set 由 denyListOwnedByCaller 要求每个 ID 存在。
		m.SkipServers = model.EnabledIDSet(m.SkipServers)
	}
	m.FailTriggerTasks = existingCronIDs(m.FailTriggerTasks)
	m.RecoverTriggerTasks = existingCronIDs(m.RecoverTriggerTasks)
}

func normalizeAlertRuleReferences(r *model.AlertRule) error {
	if len(r.Rules) > maxRulesPerAlertRule {
		return singleton.Localizer.ErrorT("too many rules, at most %d", maxRulesPerAlertRule)
	}
	for _, rule := range r.Rules {
		if rule != nil && rule.Ignore != nil {
			rule.Ignore = existingServerIDSet(rule.Ignore)
		}
	}
	r.FailTriggerTasks = existingCronIDs(r.FailTriggerTasks)
	r.RecoverTriggerTasks = existingCronIDs(r.RecoverTriggerTasks)
	return nil
}

func normalizeCronServers(cover uint8, servers []uint64) []uint64 {
	if cover == model.CronCoverAll {
		// deny-set：存在性由 checkCronServerListPermission 严格校验。
		return model.UniqueIDs(servers)
	}
	return existingServerIDs(servers)
}
