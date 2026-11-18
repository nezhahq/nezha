package model

// 资源引用集合（server / cron / DDNS profile 的 ID 列表或开关集合）的规范化。
//
// 背景（GHSA-8fgr-wx8p-v6q7）：这些集合以 JSON 形式落库，并在 AfterFind 里
// 整体反序列化、常驻内存，随后在每次列表请求时被深拷贝和序列化，部分还会在
// 运行时按条目扇出（UpdateDDNS 每个 profile 一个 goroutine 外呼，
// SendTriggerTasks 每个 task 一次 CronTrigger）。重复 ID 与值为 false 的
// 开关条目没有任何语义，却能让低权限成员把集合撑到任意大小。所以写入时必须
// 规范化，读取时也要再规范化一次，把历史上已经落库的膨胀数据收敛掉。

// UniqueIDs 按首次出现顺序去重（DDNS profile 的执行顺序有意义，不能排序）。
// nil 原样返回，保持 JSON null/[] 的既有行为。
func UniqueIDs(ids []uint64) []uint64 {
	if ids == nil {
		return nil
	}
	seen := make(map[uint64]struct{}, len(ids))
	unique := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}

// EnabledIDSet 只保留值为 true 的条目。所有消费方（DispatchTask、Rule.Skip、
// 权限校验）都只看 true 条目，false 条目等价于不存在。总是返回非 nil map。
func EnabledIDSet(set map[uint64]bool) map[uint64]bool {
	// 不按 len(set) 预分配：膨胀输入几乎全是 false 条目。
	enabled := make(map[uint64]bool)
	for id, on := range set {
		if on {
			enabled[id] = true
		}
	}
	return enabled
}

// PruneOverrideDDNSDomains 只保留绑定在 profiles 上的覆盖域名。UpdateDDNS 只会
// 按已绑定 profile 查表，其余 key 不可达却会常驻内存并随列表接口下发。
func PruneOverrideDDNSDomains(overrides map[uint64][]string, profiles []uint64) map[uint64][]string {
	if overrides == nil {
		return nil
	}
	pruned := make(map[uint64][]string)
	for _, id := range profiles {
		if domains, ok := overrides[id]; ok {
			pruned[id] = domains
		}
	}
	return pruned
}
