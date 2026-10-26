package controller

import (
	"slices"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
)

// streamAttachAllowedForRequest combines the existing creator/admin check
// with a per-request PAT whitelist gate against the stream's target server.
// Terminal and FM endpoints attach to a long-lived stream and inherit any
// authority the creator held — without the second gate an admin's PAT
// scoped to [X] could hijack a stream targeting server Y.
func streamAttachAllowedForRequest(c *gin.Context, streamId string) bool {
	if !rpc.NezhaHandlerSingleton.IsStreamAuthorizedForUser(streamId, getUid(c), callerIsAdmin(c)) {
		return false
	}
	target, ok := rpc.NezhaHandlerSingleton.StreamTarget(streamId)
	if !ok {
		return false
	}
	if !callerIsAdmin(c) && singleton.ServerControlTransitioning(target) {
		return false
	}
	return patAllowsServer(c, target)
}

func callerIsAdmin(c *gin.Context) bool {
	auth, ok := c.Get(model.CtxKeyAuthorizedUser)
	if !ok {
		return false
	}
	user, ok := auth.(*model.User)
	if !ok || user == nil {
		return false
	}
	return user.Role.IsAdmin()
}

// patAllowsServer reports whether the caller's PAT (if any) is allowed to
// touch serverID. JWT callers (no PAT in context) always pass. Used as an
// extra guard before the admin / owner short-circuits so a PAT scoped to
// a server_ids whitelist cannot widen reach via the caller's admin role.
func patAllowsServer(c *gin.Context, serverID uint64) bool {
	v, ok := c.Get(model.CtxKeyAPIToken)
	if !ok {
		return true
	}
	tok, _ := v.(model.APITokenAccessor)
	if tok == nil {
		return true
	}
	return tok.CanAccessServer(serverID)
}

// patHasServerWhitelist reports whether the caller is authenticated by a PAT
// that carries a non-empty server_ids whitelist. Cover-all semantics in
// Cron (CronCoverAll / CronCoverIgnoreAll-with-empty-Servers) and Service
// (ServiceCoverAll-with-empty-SkipServers) intentionally fan out to every
// server the cron/service's owner has — so a whitelisted PAT cannot create
// or update such configs without escaping its own whitelist. JWT callers
// and unscoped PATs have no whitelist to escape and pass through.
//
// This is the gate that turns the implicit-cover bypass at
// /api/v1/{cron,service} POST/PATCH into a 403; the dispatch side
// (CronTrigger, DispatchTask) does not re-check PAT context, so the only
// safe place to enforce it is at write time.
func patHasServerWhitelist(c *gin.Context) bool {
	v, ok := c.Get(model.CtxKeyAPIToken)
	if !ok {
		return false
	}
	wl, ok := v.(model.APITokenWhitelistView)
	if !ok || wl == nil {
		return false
	}
	return len(wl.ServerIDs()) > 0
}

// patAccessorFromContext returns the request's PAT viewed as an
// APITokenAccessor, or nil for JWT requests. Routes that need to project
// server-keyed data through the PAT whitelist (server-group, ws/server,
// future stream/list endpoints) use this instead of poking c.Get directly.
func patAccessorFromContext(c *gin.Context) model.APITokenAccessor {
	v, ok := c.Get(model.CtxKeyAPIToken)
	if !ok {
		return nil
	}
	tok, _ := v.(model.APITokenAccessor)
	if tok == nil {
		return nil
	}
	return tok
}

// checkCronServerListPermission validates the cron's Servers field. Under
// CronCoverIgnoreAll / CronCoverAlertTrigger the field is an allow-list and
// must satisfy Server.HasPermission (owner + PAT whitelist). Under
// CronCoverAll the field is a deny-list expressing exclusion; the caller
// only needs to own each listed server (PAT whitelist intersection is
// enforced separately by assertPATCoverFanoutWithinWhitelist).
func checkCronServerListPermission(c *gin.Context, cover uint8, servers []uint64, ownerUID uint64) error {
	if cover == model.CronCoverAll {
		denySet := make(map[uint64]bool, len(servers))
		for _, id := range servers {
			denySet[id] = true
		}
		if !denyListOwnedByCaller(ownerUID, denySet) {
			return singleton.Localizer.ErrorT("permission denied")
		}
		return nil
	}
	if !singleton.ServerShared.CheckPermission(c, slices.Values(servers)) {
		return singleton.Localizer.ErrorT("permission denied")
	}
	return nil
}

// checkServiceSkipServerPermission is the service-monitor analogue.
// ServiceCoverAll → SkipServers is a deny-set, only ownership required.
// ServiceCoverIgnoreAll → SkipServers is an allow-set, full Server.HasPermission.
//
// Runtime DispatchTask + skipServersToDenyList only consult entries whose
// bool value is true; false entries are no-ops. Filtering to true-only
// here keeps the write-side permission check aligned with the runtime
// fan-out (a member touching `{2: false}` for a foreign-owned server 2
// has no dispatch effect, so rejecting the request is over-restrictive
// and inconsistent with what listing / runtime see).
func checkServiceSkipServerPermission(c *gin.Context, cover uint8, skip map[uint64]bool, ownerUID uint64) error {
	effective := make(map[uint64]bool, len(skip))
	for id, enabled := range skip {
		if enabled {
			effective[id] = true
		}
	}
	if cover == model.ServiceCoverAll {
		if !denyListOwnedByCaller(ownerUID, effective) {
			return singleton.Localizer.ErrorT("permission denied")
		}
		return nil
	}
	ids := make([]uint64, 0, len(effective))
	for id := range effective {
		ids = append(ids, id)
	}
	if !singleton.ServerShared.CheckPermission(c, slices.Values(ids)) {
		return singleton.Localizer.ErrorT("permission denied")
	}
	return nil
}

// denyListOwnedByCaller verifies every id in denyList refers to a server
// owned by ownerUID. Under *CoverAll the deny-list expresses exclusion, not
// access, so it must not point at someone else's servers.
//
// Admin owners are special: runtime CronTrigger / DispatchTask fans out
// across the WHOLE system via userIsAdmin(owner), so a safe deny-list for
// an admin-owned resource must be allowed to include foreign-owned servers
// — that's the only way a limited PAT can contain the fan-out. We still
// require each id to refer to a real server, just not to be owned by the
// admin specifically.
func denyListOwnedByCaller(ownerUID uint64, denyList map[uint64]bool) bool {
	ownerIsAdmin := model.OwnerIsAdminLookup != nil && model.OwnerIsAdminLookup(ownerUID)
	for id := range denyList {
		s, found := singleton.ServerShared.Get(id)
		if !found || s == nil {
			return false
		}
		if ownerIsAdmin {
			continue
		}
		if s.GetUserID() != ownerUID {
			return false
		}
	}
	return true
}

// denyListCoversAllOwnerServersOutsidePATWhitelist reports whether every
// server visible to the cron/service owner that is NOT in the caller PAT's
// server_ids whitelist also appears in denyList. Under *CoverAll semantics
// the runtime dispatch (CronTrigger / DispatchTask) fans out to ServerShared
// minus denyList; the only way a server-limited PAT can stay inside its
// whitelist is if denyList already covers every owner-visible server outside
// that whitelist. Returning true means the configuration is safe.
func denyListCoversAllOwnerServersOutsidePATWhitelist(c *gin.Context, ownerUID uint64, denyList map[uint64]bool) bool {
	tok := patAccessorFromContext(c)
	if tok == nil {
		return true
	}
	denyIDs := make([]uint64, 0, len(denyList))
	for id, mark := range denyList {
		if mark {
			denyIDs = append(denyIDs, id)
		}
	}
	return model.DenyListSafeForLimitedPAT(tok, ownerUID, denyIDs)
}

// coverMode 抽象「cover 字段在 dispatch 时如何解读 servers 字段」。
//
// 写侧 rejectImplicit* 与运行时 manual/batch-delete 入口共用同一条 PAT 收口
// 路径（assertPATCoverFanoutWithinWhitelist），靠它把两边的规则对齐。新增任
// 何带 cover 概念的资源时，只需在自己的资源专用入口里把 Cover 枚举翻译成
// 这三档之一即可。
type coverMode uint8

const (
	// coverModePinnedByCaller: dispatch 阶段不按 servers 字段做 fan-out，
	// 真实目标在 fire 时由外部信号（如告警触发者 server）钉死。代表：
	// CronCoverAlertTrigger。PAT 在这里不做额外收口。
	coverModePinnedByCaller coverMode = iota

	// coverModeAllMinusDeny: dispatch 时取 owner 全量 server 集合，再减去
	// servers（deny-list）。代表 CronCoverAll / ServiceCoverAll。受限 PAT
	// 必须确保 deny-list 已覆盖白名单外的全部 owner servers，否则 fan-out
	// 会跑到 PAT 白名单之外。
	coverModeAllMinusDeny

	// coverModeAllowList: dispatch 时只在 servers（allow-list）内 fan-out。
	// 代表 CronCoverIgnoreAll / ServiceCoverIgnoreAll。受限 PAT 必须能访
	// 问 allow-list 中的每一个 server。空 allow-list 是「matches nothing」
	// 的退化形态，安全。
	coverModeAllowList
)

// assertPATCoverFanoutWithinWhitelist 是 cover-all / cover-ignore-all 两类
// 「按 owner 全量 fan-out」资源的 PAT 收口。
//
// 任何会按「owner servers 减 denyList」或「allowList 自身」展开的资源都必须
// 在 dispatch 入口（manual 触发 / batch-delete / mutation）调用它；写侧
// rejectImplicit* 也走同一条路径，从根上保证两边不漂移。
//
// JWT 请求或不带 server 白名单的 PAT 直接放行——它们没有「白名单」可越过。
//
// 失败时统一返回 i18n "permission denied"，与既有写侧 guard 行为一致。
func assertPATCoverFanoutWithinWhitelist(c *gin.Context, ownerUID uint64, mode coverMode, servers []uint64) error {
	if !patHasServerWhitelist(c) {
		return nil
	}
	switch mode {
	case coverModePinnedByCaller:
		return nil
	case coverModeAllMinusDeny:
		denySet := make(map[uint64]bool, len(servers))
		for _, id := range servers {
			denySet[id] = true
		}
		if !denyListCoversAllOwnerServersOutsidePATWhitelist(c, ownerUID, denySet) {
			return singleton.Localizer.ErrorT("permission denied")
		}
		return nil
	case coverModeAllowList:
		tok := patAccessorFromContext(c)
		if tok == nil {
			return nil
		}
		for _, id := range servers {
			if !tok.CanAccessServer(id) {
				return singleton.Localizer.ErrorT("permission denied")
			}
		}
		return nil
	default:
		// 未识别 cover 模式按拒绝处理；新增 coverMode 必须显式 wire 到
		// 资源专用入口里，不允许沉默放行。
		return singleton.Localizer.ErrorT("permission denied")
	}
}

// coverModeUnknown 表示 Cron/Service 持久化里出现了当前代码不认识的 cover
// 常量。这一档专门让 assertPATCoverFanoutWithinWhitelist 走 default 分支
// fail-closed，保证「未知 cover 必须显式 wire，否则拒绝」的不变量。
const coverModeUnknown coverMode = 255

// patGroupMembershipAccessAllowed returns false when the caller's PAT
// carries a server_ids whitelist that does not cover every current member
// of groupID. JWT requests and unscoped PATs always pass. Used by
// updateServerGroup before the transactional DELETE+INSERT — otherwise a
// PAT scoped to [X] could indirectly remove server Y from a shared group.
func patGroupMembershipAccessAllowed(c *gin.Context, groupID uint64) bool {
	tok := patAccessorFromContext(c)
	if tok == nil || !patHasServerWhitelist(c) {
		return true
	}
	var members []model.ServerGroupServer
	if err := singleton.DB.Where("server_group_id = ?", groupID).Find(&members).Error; err != nil {
		return false
	}
	for _, m := range members {
		if !tok.CanAccessServer(m.ServerId) {
			return false
		}
	}
	return true
}

// isValidCronCover reports whether cover is one of the runtime-recognised
// Cron Cover constants. Unknown values must be rejected at write time —
// CronTrigger's periodic scheduler path has no PAT context, so any dirty
// row persisted with an unrecognised Cover still fans out via the default
// branch (no CoverAll/IgnoreAll match → broadcast to every server passing
// cronCanSendToServer). The same allowlist applies for batch-delete and
// manual-trigger guard wiring.
func isValidCronCover(cover uint8) bool {
	switch cover {
	case model.CronCoverIgnoreAll, model.CronCoverAll, model.CronCoverAlertTrigger:
		return true
	}
	return false
}

// isValidServiceCover is the service-monitor analogue. ServiceCoverAll and
// ServiceCoverIgnoreAll are the only branches DispatchTask + Snapshot
// recognise; anything else degrades to "default fan-out" which silently
// escapes the PAT cover-fanout guard.
func isValidServiceCover(cover uint8) bool {
	switch cover {
	case model.ServiceCoverAll, model.ServiceCoverIgnoreAll:
		return true
	}
	return false
}

// cronCoverMode 把 model.CronCover* 翻译成共享底座认识的 coverMode。
//
// 未来引入新的 Cron Cover 常量时必须在这里显式 wire，否则
// assertPATCoverFanoutWithinWhitelist 会按 default 分支拒绝，避免悄悄绕过。
func cronCoverMode(cover uint8) coverMode {
	switch cover {
	case model.CronCoverAll:
		return coverModeAllMinusDeny
	case model.CronCoverIgnoreAll:
		return coverModeAllowList
	case model.CronCoverAlertTrigger:
		return coverModePinnedByCaller
	default:
		// 未识别 cover 不能降级成 pinned——pinned 会被 assert 直接放行，
		// 让受限 PAT 借未知 cover 绕过 fan-out 收口。统一报告 unknown，
		// 由 assert 的 default 分支 fail-closed。
		return coverModeUnknown
	}
}

// serviceCoverMode 是 cronCoverMode 在 service monitor 侧的对照。Service 没
// 有 alert-trigger 这一档，只有 All 与 IgnoreAll。
func serviceCoverMode(cover uint8) coverMode {
	switch cover {
	case model.ServiceCoverAll:
		return coverModeAllMinusDeny
	case model.ServiceCoverIgnoreAll:
		return coverModeAllowList
	default:
		// 同 cronCoverMode：未识别 cover 不允许借 pinned 旁路 PAT 收口。
		return coverModeUnknown
	}
}

// rejectImplicitCoverForLimitedPAT enforces the cover-all PAT guard for the
// cron write path. cf.Servers is the literal allow/deny list; under
// CronCoverAll it is a deny-list, under CronCoverIgnoreAll it is an
// allow-list, and under CronCoverAlertTrigger it does not gate dispatch at
// all (the alert trigger pins the target server at fire time). A PAT that
// carries a server_ids whitelist must therefore either (a) leave the deny-list
// empty under non-CoverAll modes — that's allow-list semantics, safe — or
// (b) under CronCoverAll, supply a deny-list that already covers every
// owner-visible server outside the PAT whitelist, otherwise CronTrigger fans
// out to those servers. Alert triggers stay unrestricted because their
// dispatch boundary is enforced by Cron.HasPermission against the trigger
// server id.
func rejectImplicitCoverForLimitedPAT(c *gin.Context, cover uint8, denyServers []uint64) error {
	return rejectImplicitCoverForLimitedPATWithOwner(c, cover, denyServers, getUid(c))
}

// rejectImplicitCoverForLimitedPATWithOwner is the explicit-owner variant
// of rejectImplicitCoverForLimitedPAT. updateCron MUST use this with the
// existing cron's UserID — not the caller — because CronTrigger fans out
// to the cron OWNER's servers at dispatch time, regardless of who issued
// the PATCH. Defaulting to getUid(c) (as rejectImplicitCoverForLimitedPAT
// does for createCron) is only safe when the caller is the owner-to-be,
// i.e. the cron is being created with cr.UserID = getUid(c).
//
// 实现层只是把参数翻译到共享底座 assertPATCoverFanoutWithinWhitelist 上；
// 写侧/运行时入口共用同一裁决，避免两边语义漂移。
func rejectImplicitCoverForLimitedPATWithOwner(c *gin.Context, cover uint8, denyServers []uint64, ownerUID uint64) error {
	// 写侧只关心 CronCoverAll 的 deny-list 是否充分——CoverIgnoreAll 的
	// allow-list 在 checkCronServerListPermission 已经过 Server.HasPermission
	// 收口；CoverAlertTrigger 在 fire 时再校验。保留这条提前 return 与
	// 老语义完全一致，避免重复 403。
	if cover != model.CronCoverAll {
		return nil
	}
	return assertPATCoverFanoutWithinWhitelist(c, ownerUID, coverModeAllMinusDeny, denyServers)
}

// rejectImplicitServiceCoverForLimitedPAT is the service-monitor analogue.
// ServiceCoverAll treats SkipServers as a deny-set: DispatchTask iterates
// ServerShared.Range and probes every server owned by the service owner that
// is NOT marked true in SkipServers. A server-limited PAT must therefore mark
// every owner-visible server outside its whitelist as skipped.
//
// 同样靠 assertPATCoverFanoutWithinWhitelist 落地，与 cron 写侧/运行时入口
// 共用一条裁决路径。
func rejectImplicitServiceCoverForLimitedPAT(c *gin.Context, cover uint8, skipServers map[uint64]bool, ownerUID uint64) error {
	if cover != model.ServiceCoverAll {
		return nil
	}
	denyServers := skipServersToDenyList(skipServers)
	return assertPATCoverFanoutWithinWhitelist(c, ownerUID, coverModeAllMinusDeny, denyServers)
}

// skipServersToDenyList 把 service monitor 用的 SkipServers map 展平成
// 共享底座需要的切片形态，并按 true 过滤。写侧/运行时入口共用，避免重复
// 写遍历逻辑。
func skipServersToDenyList(skip map[uint64]bool) []uint64 {
	out := make([]uint64, 0, len(skip))
	for id, mark := range skip {
		if mark {
			out = append(out, id)
		}
	}
	return out
}

// enforcePATCronDispatchScope 是 cron 运行时入口（manualTriggerCron /
// batchDeleteCron）的 PAT 收口。把 cr.Cover / cr.Servers 翻译成 coverMode
// 后交给共享底座；语义与写侧 rejectImplicitCoverForLimitedPAT* 严格对齐，
// 闭合「写时拦下 / 运行时回放同一条规则」的不变量，避免历史脏数据 + 受
// 限 PAT 形成越权 fan-out。
func enforcePATCronDispatchScope(c *gin.Context, cr *model.Cron) error {
	if cr == nil {
		return nil
	}
	return assertPATCoverFanoutWithinWhitelist(c, cr.GetUserID(), cronCoverMode(cr.Cover), cr.Servers)
}

// enforcePATServiceDispatchScope 是 service monitor 运行时入口
// （batchDeleteService 等）的 PAT 收口。SkipServers 是 map[uint64]bool，
// 这里展开成 deny-list 切片喂给共享底座；语义与
// rejectImplicitServiceCoverForLimitedPAT 严格对齐。
func enforcePATServiceDispatchScope(c *gin.Context, svc *model.Service) error {
	if svc == nil {
		return nil
	}
	return assertPATCoverFanoutWithinWhitelist(c, svc.GetUserID(), serviceCoverMode(svc.Cover), skipServersToDenyList(svc.SkipServers))
}

// enforcePATTriggerTaskScope 阻止 service:write / alertrule:write 的 PAT 通过绑定
// trigger task 越权执行 cron。运行时 alertsentinel/servicesentinel 触发
// CronShared.SendTriggerTasks 时没有 PAT 上下文，CheckPermission 也只校验
// ownership/白名单而非 scope，所以必须在写侧对 PAT 额外要求 ScopeCronExec。
func enforcePATTriggerTaskScope(c *gin.Context, failTasks, recoverTasks []uint64) error {
	if len(failTasks) == 0 && len(recoverTasks) == 0 {
		return nil
	}
	tok := APITokenFromContext(c)
	if tok == nil {
		return nil
	}
	if !tok.HasScope(model.ScopeCronExec) {
		return singleton.Localizer.ErrorT("permission denied")
	}
	return nil
}

func userCanViewServer(c *gin.Context, server *model.Server) bool {
	if server == nil {
		return false
	}
	// PAT 白名单优先于 admin/owner 早返回：admin 自己签发的 server_ids 受限 PAT
	// 必须只能看见白名单里的 server，否则给自己设的硬边界形同虚设。
	if !patAllowsServer(c, server.GetID()) {
		return false
	}
	if callerIsAdmin(c) {
		return true
	}
	if _, isMember := c.Get(model.CtxKeyAuthorizedUser); isMember {
		if server.HasPermission(c) {
			return true
		}
		return !server.HideForGuest
	}
	return !server.HideForGuest
}

func userCanViewService(c *gin.Context, service *model.Service) bool {
	if service == nil {
		return false
	}
	// HideForGuest 默认公开，置 true 才对 guest 隐藏，语义与 Server.HideForGuest 对齐。
	if service.HideForGuest {
		if _, isMember := c.Get(model.CtxKeyAuthorizedUser); !isMember {
			return false
		}
		// 必须先让 Service.HasPermission 跑 PAT 白名单收口，再让 admin 在无 PAT 请求上
		// 短路放行，否则 admin 自签的受限 PAT 会被早返回绕过 list/history 的 PAT 边界。
		return service.HasPermission(c)
	}
	return true
}

func assertOwnsNotificationGroup(c *gin.Context, groupID uint64) error {
	if groupID == 0 {
		return nil
	}

	var ng model.NotificationGroup
	if err := singleton.DB.First(&ng, groupID).Error; err != nil {
		return singleton.Localizer.ErrorT("notification group id %d does not exist", groupID)
	}
	if !ng.HasPermission(c) {
		return singleton.Localizer.ErrorT("permission denied")
	}
	return nil
}
