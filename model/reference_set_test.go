package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 回归测试：GHSA-8fgr-wx8p-v6q7。升级前已经落库的膨胀集合在加载时必须被收敛，
// 不能原样常驻内存并被运行时按条目扇出。

func TestUniqueIDsPreservesFirstOccurrenceOrder(t *testing.T) {
	require.Nil(t, UniqueIDs(nil))
	require.Equal(t, []uint64{}, UniqueIDs([]uint64{}))
	require.Equal(t, []uint64{3, 1, 2}, UniqueIDs([]uint64{3, 1, 3, 2, 1, 3}))
}

func TestEnabledIDSetDropsFalseEntries(t *testing.T) {
	require.Equal(t, map[uint64]bool{}, EnabledIDSet(nil))
	require.Equal(t, map[uint64]bool{2: true}, EnabledIDSet(map[uint64]bool{1: false, 2: true, 3: false}))
}

func TestServerAfterFindNormalizesPersistedDDNSReferences(t *testing.T) {
	s := &Server{
		DDNSProfilesRaw:        `[8,7,8,7,7]`,
		OverrideDDNSDomainsRaw: `{"7":["a.example"],"99":["unbound.example"]}`,
	}
	require.NoError(t, s.AfterFind(nil))
	require.Equal(t, []uint64{8, 7}, s.DDNSProfiles)
	require.Equal(t, map[uint64][]string{7: {"a.example"}}, s.OverrideDDNSDomains)
}

func TestServiceAfterFindNormalizesPersistedReferences(t *testing.T) {
	m := &Service{
		SkipServersRaw:         `{"1":true,"2":false,"3":false}`,
		FailTriggerTasksRaw:    `[5,5,6,5]`,
		RecoverTriggerTasksRaw: `[6,6]`,
	}
	require.NoError(t, m.AfterFind(nil))
	require.Equal(t, map[uint64]bool{1: true}, m.SkipServers)
	require.Equal(t, []uint64{5, 6}, m.FailTriggerTasks)
	require.Equal(t, []uint64{6}, m.RecoverTriggerTasks)
}

func TestCronAfterFindDeduplicatesPersistedServers(t *testing.T) {
	c := &Cron{ServersRaw: `[2,1,2,1]`}
	require.NoError(t, c.AfterFind(nil))
	require.Equal(t, []uint64{2, 1}, c.Servers)
}

func TestAlertRuleAfterFindNormalizesPersistedReferences(t *testing.T) {
	r := &AlertRule{
		RulesRaw:               `[{"type":"cpu","cover":0,"ignore":{"1":true,"2":false}}]`,
		FailTriggerTasksRaw:    `[5,5]`,
		RecoverTriggerTasksRaw: `[6,5,6]`,
	}
	require.NoError(t, r.AfterFind(nil))
	require.Equal(t, map[uint64]bool{1: true}, r.Rules[0].Ignore)
	require.Equal(t, []uint64{5}, r.FailTriggerTasks)
	require.Equal(t, []uint64{6, 5}, r.RecoverTriggerTasks)
}
