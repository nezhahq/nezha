package controller

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
)

// 旧 agent 不识别 TaskTypeExec/TaskTypeFs* 等新 task type，会走 default
// 分支不回 TaskResult；dashboard 必须在调 CallAgent 之前依据 Host.Version
// 快速失败，否则用户要等到 30s/24h timeout 才知道 agent 不支持。
// MinServerTransferAgentVersion 已为同类问题在 transfer 路径上确立了
// release-time 必填的版本下限——这里把 MCP 也纳入同一不变量。
func TestMCPMinAgentVersionIsPinnedToRelease(t *testing.T) {
	require.NotEmpty(t, MCPMinAgentVersion,
		"MCPMinAgentVersion must be set to the lowest agent build that ships MCP handlers; an empty string disables the gate and lets old agents hang dashboard requests until timeout")
}

func TestRequireAgentSupportsMCPRejectsBelowMinVersion(t *testing.T) {
	old := &model.Server{Host: &model.Host{Version: "v0.0.1"}}
	err := requireAgentSupportsMCP(old)
	require.Error(t, err, "agents older than MCPMinAgentVersion must be rejected before CallAgent")
	require.True(t, errors.Is(err, errMCPUnsupported) || err.Error() == errMCPUnsupported.Error(),
		"expected the errMCPUnsupported sentinel, got %v", err)
}

func TestRequireAgentSupportsMCPAcceptsCurrentVersion(t *testing.T) {
	current := &model.Server{Host: &model.Host{Version: MCPMinAgentVersion}}
	require.NoError(t, requireAgentSupportsMCP(current),
		"server reporting exactly MCPMinAgentVersion must be accepted")
}

// 钉住「最近一个不带 MCP handler 的已发布 agent tag (v2.0.4) 必须被拒绝」。
// v2.0.4 的 model/task.go 还没有 TaskTypeExec/TaskTypeFs* 常量，cmd/agent/
// mcp_handlers.go 也不存在；如果版本门槛把它放行，dashboard 调 MCP tool
// 后 agent 会走 default 分支不回 TaskResult，CallAgent 必须等 30s 超时。
func TestRequireAgentSupportsMCPRejectsLastReleaseWithoutMCP(t *testing.T) {
	noMCP := &model.Server{Host: &model.Host{Version: "v2.0.4"}}
	err := requireAgentSupportsMCP(noMCP)
	require.Error(t, err,
		"v2.0.4 is the latest released agent tag that ships *without* MCP handlers; bumping MCPMinAgentVersion below the first MCP release re-introduces the silent-timeout bug")
	require.True(t, errors.Is(err, errMCPUnsupported) || err.Error() == errMCPUnsupported.Error(),
		"expected the errMCPUnsupported sentinel, got %v", err)
}

func TestRequireAgentSupportsMCPDefersWhenAgentNeverReported(t *testing.T) {
	require.NoError(t, requireAgentSupportsMCP(&model.Server{Host: nil}),
		"Host==nil means agent never reported its build; defer the version decision to the CallAgent timeout layer")
}

func TestCompareSemverIgnoresVPrefixMismatch(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.1.0", "v2.1.0", 0},
		{"v2.1.0", "2.1.0", 0},
		{"2.1.0", "2.1.0", 0},
		{"2.1.0", "v2.1.1", -1},
		{"v2.1.2", "2.1.0", 1},
		{"2.2.0", "v2.1.9", 1},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, compareSemver(c.a, c.b),
			"compareSemver(%q,%q): the only difference is the optional 'v' prefix and/or numeric ordering; "+
				"a bare lexical fallback wrongly orders %q < %q", c.a, c.b, c.a, c.b)
	}
}

func TestRequireAgentSupportsMCPAcceptsBarePrefixReport(t *testing.T) {
	agent := &model.Server{Host: &model.Host{Version: "2.1.0"}}
	require.NoError(t, requireAgentSupportsMCP(agent),
		"agents report Host.Version without the 'v' prefix (e.g. \"2.1.0\"); it must compare equal to MCPMinAgentVersion \"v2.1.0\" and be accepted")
}

// Host.Version 由 agent 任意上报，长度只受 gRPC 消息上限约束；版本门槛在每次
// MCP tool 调用时都会解析它，所以解析的内存开销不能随版本串长度放大。
func TestSemverPartsAllocationIsIndependentOfVersionLength(t *testing.T) {
	agentChosen := "2.1.0" + strings.Repeat(".", 4<<20)
	// TotalAlloc 是进程级累计值，取三次中的最小值以排除其他 goroutine 的分配噪声。
	allocated := ^uint64(0)
	for range 3 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got := semverParts(agentChosen)
		runtime.ReadMemStats(&after)
		require.Equal(t, [3]int{2, 1, 0}, got)
		allocated = min(allocated, after.TotalAlloc-before.TotalAlloc)
	}
	require.Less(t, allocated, uint64(len(agentChosen)),
		"semverParts only reads the first three components; splitting the whole agent-supplied version costs 16 bytes per dot on every MCP tool call")
	require.NoError(t, requireAgentSupportsMCP(&model.Server{Host: &model.Host{Version: agentChosen}}))
}

func TestSemverPartsIgnoresComponentsAfterPatch(t *testing.T) {
	cases := []struct {
		v    string
		want [3]int
	}{
		{"", [3]int{}},
		{"2", [3]int{2, 0, 0}},
		{"2.1", [3]int{2, 1, 0}},
		{"v2.1.3", [3]int{2, 1, 3}},
		{"2.1.3.4.5", [3]int{2, 1, 3}},
		{"2.1.3-rc.1+build.7", [3]int{2, 1, 3}},
		{"2.1.3rc.9.9", [3]int{2, 1, 3}},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, semverParts(c.v), "semverParts(%q)", c.v)
	}
}
