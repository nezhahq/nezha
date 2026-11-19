package model

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Scope 命名规范（唯一一套）：nezha:{resource}:{verb}
//
//   - resource: inventory / server / service / alertrule / cron / ddns / nat /
//     notification / notification-group / transfer / admin
//   - verb: read / write / delete / exec
//
// `*` 通配在 resource 或 verb 位均可：
//   - nezha:server:* 给定资源的所有动作
//   - nezha:* admin-only 全权
//
// inventory 与 server 已拆开：inventory 管“能看到/能删哪些机器”——`server.list`
// MCP tool、`GET /api/v1/server`、`/server-group`、batch-delete server/group 都用
// nezha:inventory:{read,delete}；server 管对已知机器的运行态操作（server.get、
// exec、文件读写、编辑配置、metrics）。同一 scope 同时管 MCP tool 和 REST endpoint。
//
// 历史上还有 mcp:* 一套，会被 HasScope 通过别名映射到 nezha:server:* 子集。
// 由于 HasScope 同时服务 MCP tool 调度与 REST scope middleware，旧 mcp:fs:write
// 等会静默扩到所有 nezha:server:write REST 路由——这是命名分裂带来的提权漏洞。
// 现在 mcp:* 不再在运行时被识别；createAPIToken 入口对老调用方做一次性归一化：
// 只读/exec 类（mcp:fs:read、mcp:server:read、mcp:server:exec）映射到对应
// nezha:* read/exec scope；mcp:fs:write、mcp:fs:delete、mcp:* 一律拒签。
// 数据库已有的危险旧 scope 由 MigrateLegacyMCPScopes 在启动迁移阶段清理。
const (
	ScopeNezhaAll = "nezha:*"

	// inventory 资源域：管理后台对“服务器清单”本身的枚举与删除（列出 GET /server、
	// 删除 batch-delete/server、server-group 的列出/删除，以及 MCP server.list）。
	// 刻意与 nezha:server:* 分开：后者是对已知 server 的运行态操作（exec / 文件读写 /
	// 编辑 / metrics），而 inventory 是“能看到/能删哪些机器”的台账权限。拆开后，
	// 一张只跑命令的 PAT 不必同时具备遍历和删除整个清单的能力。
	ScopeInventoryRead   = "nezha:inventory:read"
	ScopeInventoryDelete = "nezha:inventory:delete"

	ScopeServerRead   = "nezha:server:read"
	ScopeServerWrite  = "nezha:server:write"
	ScopeServerDelete = "nezha:server:delete"
	ScopeServerExec   = "nezha:server:exec"

	ScopeServiceRead   = "nezha:service:read"
	ScopeServiceWrite  = "nezha:service:write"
	ScopeServiceDelete = "nezha:service:delete"

	ScopeAlertRuleRead   = "nezha:alertrule:read"
	ScopeAlertRuleWrite  = "nezha:alertrule:write"
	ScopeAlertRuleDelete = "nezha:alertrule:delete"

	ScopeCronRead   = "nezha:cron:read"
	ScopeCronWrite  = "nezha:cron:write"
	ScopeCronDelete = "nezha:cron:delete"
	ScopeCronExec   = "nezha:cron:exec"

	ScopeDDNSRead   = "nezha:ddns:read"
	ScopeDDNSWrite  = "nezha:ddns:write"
	ScopeDDNSDelete = "nezha:ddns:delete"

	ScopeNATRead   = "nezha:nat:read"
	ScopeNATWrite  = "nezha:nat:write"
	ScopeNATDelete = "nezha:nat:delete"

	ScopeNotificationRead   = "nezha:notification:read"
	ScopeNotificationWrite  = "nezha:notification:write"
	ScopeNotificationDelete = "nezha:notification:delete"

	ScopeNotificationGroupRead   = "nezha:notification-group:read"
	ScopeNotificationGroupWrite  = "nezha:notification-group:write" // #nosec G101 -- scope identifier, not a credential
	ScopeNotificationGroupDelete = "nezha:notification-group:delete"

	ScopeTransferRead   = "nezha:transfer:read"
	ScopeTransferWrite  = "nezha:transfer:write"
	ScopeTransferDelete = "nezha:transfer:delete"

	ScopeAdminAll = "nezha:admin:*"
)

var AllScopes = []string{
	ScopeInventoryRead, ScopeInventoryDelete,
	ScopeServerRead, ScopeServerWrite, ScopeServerDelete, ScopeServerExec,
	ScopeServiceRead, ScopeServiceWrite, ScopeServiceDelete,
	ScopeAlertRuleRead, ScopeAlertRuleWrite, ScopeAlertRuleDelete,
	ScopeCronRead, ScopeCronWrite, ScopeCronDelete, ScopeCronExec,
	ScopeDDNSRead, ScopeDDNSWrite, ScopeDDNSDelete,
	ScopeNATRead, ScopeNATWrite, ScopeNATDelete,
	ScopeNotificationRead, ScopeNotificationWrite, ScopeNotificationDelete,
	ScopeNotificationGroupRead, ScopeNotificationGroupWrite, ScopeNotificationGroupDelete,
	ScopeTransferRead, ScopeTransferWrite, ScopeTransferDelete,

	"nezha:inventory:*",
	"nezha:server:*",
	"nezha:service:*",
	"nezha:alertrule:*",
	"nezha:cron:*",
	"nezha:ddns:*",
	"nezha:nat:*",
	"nezha:notification:*",
	"nezha:notification-group:*",
	"nezha:transfer:*",
}

var AdminOnlyScopes = []string{ScopeNezhaAll, ScopeAdminAll}

// legacyMCPReadOnlyRewrite 列出仍允许 createAPIToken 入口重写为 nezha:* 的旧 scope。
// 只有只读/exec 类被接受；write/delete/wildcard 一律拒签——保留映射等于扩权。
var legacyMCPReadOnlyRewrite = map[string]string{
	"mcp:server:read": ScopeServerRead,
	"mcp:server:exec": ScopeServerExec,
	"mcp:fs:read":     ScopeServerRead,
}

// NormalizeIncomingScope 把入参里的旧 mcp:* scope 重写到 nezha:* 命名。
// 第二个返回值表示该 scope 是否被允许（false = 危险旧 scope，调用方应拒签）。
func NormalizeIncomingScope(s string) (string, bool) {
	if mapped, ok := legacyMCPReadOnlyRewrite[s]; ok {
		return mapped, true
	}
	if strings.HasPrefix(s, "mcp:") {
		return s, false
	}
	return s, true
}

// APITokenPrefix 是明文 token 的人类可识别前缀。`nzp_` = nezha personal access token。
const APITokenPrefix = "nzp_"

// APIToken 是用户用于程序化访问的长期凭证。MCP 接入点 /mcp 用它做鉴权。
// 双层鉴权：闸 1 用 UserID 复用 Server.HasPermission；闸 2 用 Scopes / ServerIDs。
type APIToken struct {
	ID         uint64     `gorm:"primaryKey" json:"id,omitempty"`
	UserID     uint64     `gorm:"index" json:"user_id,omitempty"`
	Name       string     `gorm:"type:varchar(128)" json:"name,omitempty"`
	TokenHash  string     `gorm:"uniqueIndex;type:char(64)" json:"-"`
	ScopesCSV  string     `gorm:"type:text" json:"-"`
	ServersCSV string     `gorm:"type:text" json:"-"`
	ExpiresAt  *time.Time `gorm:"index" json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP string     `gorm:"type:varchar(64)" json:"last_used_ip,omitempty"`
	CreatedAt  time.Time  `json:"created_at,omitempty"`
	UpdatedAt  time.Time  `gorm:"autoUpdateTime" json:"updated_at,omitempty"`

	// serverWhitelist is ServersCSV decoded once (AfterFind / SetServerIDs) so
	// that CanAccessServer does not parse the CSV again on every call.
	serverWhitelist *apiTokenServerWhitelist

	// UserVersion is the owner's User.TokenVersion at issue time. A credential
	// change bumps that counter and retires the token, like JWT sessions and
	// MCP OAuth grants.
	UserVersion uint64 `gorm:"not null;default:0" json:"-"`
}

// apiTokenServerWhitelist is an immutable decoded ServersCSV. It remembers the
// CSV it was built from, so a token whose ServersCSV was assigned directly is
// never answered from a stale whitelist.
type apiTokenServerWhitelist struct {
	csv string
	ids []uint64 // sorted
}

func (t *APIToken) decodeServerWhitelist() *apiTokenServerWhitelist {
	ids := t.ServerIDs()
	slices.Sort(ids)
	return &apiTokenServerWhitelist{csv: t.ServersCSV, ids: ids}
}

func (APIToken) TableName() string {
	return "api_tokens"
}

// HashAPIToken 计算明文 token 的存储哈希。
func HashAPIToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Scopes 解码逗号分隔的 scope 列表。
func (t *APIToken) Scopes() []string {
	if t.ScopesCSV == "" {
		return nil
	}
	parts := strings.Split(t.ScopesCSV, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SetScopes 编码 scope 列表为 CSV。
func (t *APIToken) SetScopes(scopes []string) {
	t.ScopesCSV = strings.Join(scopes, ",")
}

// ServerIDs 解码服务器 ID 白名单。空切片 = 不限制（继承用户原有权限）。
func (t *APIToken) ServerIDs() []uint64 {
	if t.ServersCSV == "" {
		return nil
	}
	parts := strings.Split(t.ServersCSV, ",")
	out := make([]uint64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var id uint64
		for _, c := range p {
			if c < '0' || c > '9' {
				id = 0
				break
			}
			id = id*10 + uint64(c-'0')
		}
		if id != 0 {
			out = append(out, id)
		}
	}
	return out
}

// SetServerIDs 编码服务器 ID 白名单。
func (t *APIToken) SetServerIDs(ids []uint64) {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, formatUint(id))
	}
	t.ServersCSV = strings.Join(parts, ",")
	t.serverWhitelist = t.decodeServerWhitelist()
}

// AfterFind 解码一次服务器 ID 白名单，供 CanAccessServer 复用。
func (t *APIToken) AfterFind(tx *gorm.DB) error {
	t.serverWhitelist = t.decodeServerWhitelist()
	return nil
}

// HasScope 判定 token 是否携带某个 scope。
//
// 匹配规则：
//   - nezha:* 覆盖整个 nezha 命名空间
//   - 资源级通配：nezha:server:* 匹配所有 nezha:server:read/write/delete/exec
//   - 精确匹配
//
// 不再做 mcp:* 别名展开；任何遗留的 mcp:* scope 都视为无效（已被
// MigrateLegacyMCPScopes 在启动迁移阶段清掉；运行时再遇到当作无权处理）。
func (t *APIToken) HasScope(scope string) bool {
	for _, s := range t.Scopes() {
		if scopeMatches(s, scope) {
			return true
		}
	}
	return false
}

// scopeMatches 判定 owned scope 是否覆盖 wanted scope。
func scopeMatches(owned, wanted string) bool {
	if owned == wanted {
		return true
	}
	if owned == ScopeNezhaAll {
		return strings.HasPrefix(wanted, "nezha:")
	}
	if strings.HasSuffix(owned, ":*") {
		prefix := strings.TrimSuffix(owned, ":*")
		return strings.HasPrefix(wanted, prefix+":") || wanted == prefix
	}
	return false
}

// CanAccessServer 判定 token 是否被允许操作某 server（白名单层；
// 仍需上层调用 Server.HasPermission 做用户级权限校验）。
//
// 列表/推流类调用方会对每台 server 调用一次，所以这里查已解码的白名单，
// 而不是每次调用都重新解析 ServersCSV。
func (t *APIToken) CanAccessServer(serverID uint64) bool {
	if t.ServersCSV == "" {
		return true
	}
	wl := t.serverWhitelist
	if wl == nil || wl.csv != t.ServersCSV {
		wl = t.decodeServerWhitelist()
	}
	if len(wl.ids) == 0 {
		return true
	}
	_, found := slices.BinarySearch(wl.ids, serverID)
	return found
}

// IsExpired 判定 token 是否已过期。ExpiresAt 为 nil 表示永不过期。
func (t *APIToken) IsExpired(now time.Time) bool {
	return t.ExpiresAt != nil && now.After(*t.ExpiresAt)
}

// BeforeCreate 在写入前强校验 TokenHash 必填，避免空哈希撞键。
func (t *APIToken) BeforeCreate(tx *gorm.DB) error {
	if t.TokenHash == "" {
		return gorm.ErrInvalidData
	}
	return nil
}

// MigrateLegacyMCPScopes 把数据库里残留的 mcp:* scope 一次性归一化：
//   - 只读/exec 类映射到对应 nezha:* read/exec scope；
//   - mcp:fs:write / mcp:fs:delete / mcp:* 会被剥掉（不再赋予 REST write/delete），
//     若 token 因此 scope 列表清空则整体删除——避免出现一张 0 scope 但仍能命中
//     auth middleware 的 PAT。
//
// 返回 (rewrittenTokens, deletedTokens, err)。生产路径在启动时调用一次；
// 测试也会用它构造 fixture。
func MigrateLegacyMCPScopes(db *gorm.DB) (int, int, error) {
	if db == nil {
		return 0, 0, nil
	}
	var rows []APIToken
	if err := db.Where("scopes_csv LIKE ?", "%mcp:%").Find(&rows).Error; err != nil {
		return 0, 0, err
	}
	rewritten, deleted := 0, 0
	for i := range rows {
		tok := &rows[i]
		old := tok.Scopes()
		next := make([]string, 0, len(old))
		seen := make(map[string]struct{}, len(old))
		for _, s := range old {
			mapped, ok := NormalizeIncomingScope(s)
			if !ok {
				continue
			}
			if _, dup := seen[mapped]; dup {
				continue
			}
			seen[mapped] = struct{}{}
			next = append(next, mapped)
		}
		if len(next) == 0 {
			if err := db.Delete(&APIToken{}, tok.ID).Error; err != nil {
				return rewritten, deleted, err
			}
			deleted++
			continue
		}
		joined := strings.Join(next, ",")
		if joined == tok.ScopesCSV {
			continue
		}
		if err := db.Model(&APIToken{}).Where("id = ?", tok.ID).
			Update("scopes_csv", joined).Error; err != nil {
			return rewritten, deleted, err
		}
		rewritten++
	}
	return rewritten, deleted, nil
}

// formatUint —— 小工具，避免引入 strconv。
func formatUint(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// APITokenCreateRequest 是创建 PAT 接口的入参。
type APITokenCreateRequest struct {
	Name          string   `json:"name" binding:"required,max=128"`
	Scopes        []string `json:"scopes" binding:"required,min=1,dive,max=64"`
	ServerIDs     []uint64 `json:"server_ids,omitempty"`
	ExpiresInDays int      `json:"expires_in_days,omitempty"` // 0 = 永不过期
}

// APITokenCreateResponse 创建 PAT 接口的出参；明文 token 仅在此刻返回一次。
type APITokenCreateResponse struct {
	ID        uint64     `json:"id"`
	Name      string     `json:"name"`
	Token     string     `json:"token"`
	Scopes    []string   `json:"scopes"`
	ServerIDs []uint64   `json:"server_ids,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// APITokenView 是 PAT 列表展示用的脱敏视图。
type APITokenView struct {
	ID         uint64     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	ServerIDs  []uint64   `json:"server_ids,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP string     `json:"last_used_ip,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ToView 把数据库实体转为列表脱敏视图。
func (t *APIToken) ToView() APITokenView {
	return APITokenView{
		ID:         t.ID,
		Name:       t.Name,
		Scopes:     t.Scopes(),
		ServerIDs:  t.ServerIDs(),
		ExpiresAt:  t.ExpiresAt,
		LastUsedAt: t.LastUsedAt,
		LastUsedIP: t.LastUsedIP,
		CreatedAt:  t.CreatedAt,
	}
}
