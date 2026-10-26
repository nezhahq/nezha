package singleton

import (
	"cmp"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
)

// GHSA-x6fg-52vr-hj4w: NAT 是 commonHandler，任意认证成员可创建。newHTTPandGRPCMux
// 在分发 dashboard/gRPC 之前先按 r.Host 命中 NAT，故成员若把 Domain 设成 dashboard
// 自身 host 即可抢占全局路由（disabled 触发 DoS，enabled 把请求隧道到攻击者 agent）。
// 把 dashboard 的 InstallHost、ListenHost 以及运维声明的 ReservedHosts 列为
// 保留 host：create/update 时拒绝，启动建表时丢弃，确保补丁前已植入的恶意记录
// 在升级后不再生效。每个 host 拆成 hostname 后比较（忽略端口与大小写），反代/
// 默认端口下的端口变体也拦得住。反代部署时进程看不到对外域名，运维把它配进
// Conf.ReservedHosts（逗号分隔）即可让此处覆盖到公网入口。
// NATDomainAllowed requires an operator-assigned exact hostname for members.
// Comparing only request.Host is bypassable via an alternate dashboard entrance.
func NATDomainAllowed(domain string, admin bool) bool {
	if Conf == nil || Conf.Config == nil {
		return false
	}
	policy := Conf.NATDomainPolicySnapshot()
	if policy.isReserved(domain) {
		return false
	}
	if admin {
		return true
	}
	if splitDashboardHostname(policy.DashboardHost) == "" {
		return false
	}
	target := splitDashboardHostname(domain)
	if target == "" {
		return false
	}
	for _, allowed := range policy.MemberDomains {
		if splitDashboardHostname(allowed) == target {
			return true
		}
	}
	return false
}

func IsReservedDashboardHost(domain string) bool {
	if Conf == nil || Conf.Config == nil {
		return false
	}
	return Conf.NATDomainPolicySnapshot().isReserved(domain)
}

// splitDashboardHostname 归一化为小写 hostname，对 bracketed IPv6（[::1]、
// [::1]:8008）与裸 host:port 一视同仁，避免 candidate 与 reserved 解析形态不
// 一致导致漏拦。两类等价形态也必须收敛，否则 guard 放行而 r.Host 精确命中
// 仍能劫持路由：
//   - DNS absolute name 的尾点（panel.example.com. 与 panel.example.com 指向同
//     一主机），去掉单个尾点；
//   - IP literal 的压缩/展开写法（::1 与 0:0:0:0:0:0:0:1），用 netip 归一到
//     规范文本。
func splitDashboardHostname(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		host = h
	} else {
		host = strings.Trim(host, "[]")
	}
	host = strings.TrimSuffix(host, ".")
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.String()
	}
	return host
}

// filterReservedNATProfiles 丢弃 Domain 命中 dashboard 保留 host 的 NAT 记录，
// 让 NewNATClass 启动建表时不把补丁前植入的劫持记录加载进路由表。
func filterReservedNATProfiles(in []*model.NAT) []*model.NAT {
	out := in[:0]
	for _, profile := range in {
		if profile == nil || IsReservedDashboardHost(profile.Domain) {
			continue
		}
		out = append(out, profile)
	}
	return out
}

type NATClass struct {
	class[string, *model.NAT]

	idToDomain map[uint64]string
}

func NewNATClass() *NATClass {
	var sortedList []*model.NAT

	DB.Find(&sortedList)
	sortedList = filterReservedNATProfiles(sortedList)
	list := make(map[string]*model.NAT, len(sortedList))
	idToDomain := make(map[uint64]string, len(sortedList))
	for _, profile := range sortedList {
		list[profile.Domain] = profile
		idToDomain[profile.ID] = profile.Domain
	}

	return &NATClass{
		class: class[string, *model.NAT]{
			list:       list,
			sortedList: sortedList,
		},
		idToDomain: idToDomain,
	}
}

func (c *NATClass) Update(n *model.NAT) {
	c.listMu.Lock()

	if oldDomain, ok := c.idToDomain[n.ID]; ok && oldDomain != n.Domain {
		delete(c.list, oldDomain)
	}

	c.list[n.Domain] = n
	c.idToDomain[n.ID] = n.Domain

	c.listMu.Unlock()
	c.sortList()
}

func (c *NATClass) Delete(idList []uint64) {
	c.listMu.Lock()

	for _, id := range idList {
		if domain, ok := c.idToDomain[id]; ok {
			delete(c.list, domain)
			delete(c.idToDomain, id)
		}
	}

	c.listMu.Unlock()
	c.sortList()
}

func (c *NATClass) GetNATConfigByDomain(domain string) *model.NAT {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	// Runtime reservations must also invalidate routes already loaded in memory.
	if IsReservedDashboardHost(domain) {
		return nil
	}
	profile := c.list[domain]
	if profile == nil {
		return nil
	}
	UserLock.RLock()
	owner, known := UserInfoMap[profile.UserID]
	UserLock.RUnlock()
	if !known || !NATDomainAllowed(domain, owner.Role.IsAdmin()) || ServerShared == nil {
		return nil
	}
	server, exists := ServerShared.Get(profile.ServerID)
	// Check even disabled profiles before the mux can let them shadow the panel.
	if !exists || (!owner.Role.IsAdmin() && (server.GetUserID() != profile.UserID || ServerControlTransitioning(server.ID))) {
		return nil
	}
	return profile
}

func (c *NATClass) GetDomain(id uint64) string {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	return c.idToDomain[id]
}

func (c *NATClass) sortList() {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	sortedList := utils.MapValuesToSlice(c.list)
	slices.SortFunc(sortedList, func(a, b *model.NAT) int {
		return cmp.Compare(a.ID, b.ID)
	})

	c.sortedListMu.Lock()
	defer c.sortedListMu.Unlock()
	c.sortedList = sortedList
}
