package singleton

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKomariAPIShimInterceptsPublicReadOnlyContracts(t *testing.T) {
	shim := string(KomariAPIShimJS())
	for _, needle := range []string{
		"/api/rpc2",
		"/api/public",
		"/api/me",
		"/api/recent/",
		"/api/records/load",
		"/api/records/ping",
		"rpc.ping",
		"rpc.getVersion",
		"rpc.getMethods",
		"rpc.getHelp",
		"common:getBackendVersion",
		"common:getNodes",
		"common:getNodesLatestStatus",
		"public:getPublicSettings",
		"public:getNodesInformation",
		"public:getClientRecentRecords",
		"public:getRecordsByUUID",
		"public:queryMetrics",
		"public:getPingRecords",
		"public:getPublicPingTasks",
	} {
		require.Contains(t, shim, needle)
	}
	require.Contains(t, shim, "class KomariRPCWebSocket")
	require.Contains(t, shim, "window.fetch")
	require.Contains(t, shim, "window.XMLHttpRequest")
	require.Contains(t, shim, "class KomariXMLHttpRequest")
	require.Contains(t, shim, "window.WebSocket")
	require.Contains(t, shim, "mergeNezhaSnapshot")
	require.Contains(t, shim, "maxMetricRequests")
	require.Contains(t, shim, "u.origin === location.origin && u.pathname === '/api/rpc2'")
	require.Contains(t, shim, "u.origin === location.origin && u.pathname === '/api/clients'")
	require.Contains(t, shim, "/api/admin/theme/settings")
	require.Contains(t, shim, "redirectKomariAdmin")
	require.Contains(t, shim, "history.pushState")
	require.Contains(t, shim, "'/dashboard'")
	require.Contains(t, shim, "isKomariLoginControl")
	require.Contains(t, shim, "aria-label")
	for _, label := range []string{"login", "sign in", "登录", "登入", "ログイン"} {
		require.Contains(t, shim, label)
	}
}

func TestKomariAPIShimUsesOnlyNezhaGuestReadEndpoints(t *testing.T) {
	shim := string(KomariAPIShimJS())
	for _, allowed := range []string{
		"/api/v1/setting",
		"/api/v1/server-group",
		"/api/v1/ws/server",
		"/api/v1/server/",
	} {
		require.Contains(t, shim, allowed)
	}
	for _, forbidden := range []string{
		"/api/v1/terminal",
		"/api/v1/file",
		"/api/v1/batch-delete",
		"Authorization: Bearer",
		"localStorage.getItem('token')",
	} {
		require.NotContains(t, shim, forbidden)
	}
}

func TestKomariAPIShimDoesNotFakeAdminWrites(t *testing.T) {
	shim := string(KomariAPIShimJS())
	require.Contains(t, shim, "readonlyError")
	require.Contains(t, shim, "-32601")
	require.NotContains(t, shim, "admin:update")
}

func TestInjectKomariCompatibilityRunsBeforeThemeAssets(t *testing.T) {
	html := []byte(`<html><head><script type="module" src="/assets/theme.js"></script></head><body></body></html>`)
	out := string(InjectKomariCompatibility(html, "market-theme", "https://cdn.example/wall.webp"))
	shimAt := strings.Index(out, "nezha-komari-api-shim")
	themeAt := strings.Index(out, `/assets/theme.js`)
	require.GreaterOrEqual(t, shimAt, 0)
	require.Greater(t, themeAt, shimAt)
	require.Contains(t, out, "https://cdn.example/wall.webp")
}

func TestKomariShimNodeFixtureConversion(t *testing.T) {
	serverJSON := fmt.Sprintf(`{"id":7,"name":"alpha","host":{"platform":"linux","cpu":["AMD"],"mem_total":1024,"disk_total":2048,"swap_total":128,"arch":"amd64","virtualization":"kvm"},"state":{"cpu":25.5,"mem_used":512,"disk_used":1000,"swap_used":64,"net_in_speed":11,"net_out_speed":22,"net_in_transfer":33,"net_out_transfer":44,"uptime":55,"load_1":1.2,"load_5":0.8,"load_15":0.4,"tcp_conn_count":5,"udp_conn_count":2,"process_count":99},"country_code":"JP","last_active":%q}`, time.Now().UTC().Format(time.RFC3339))
	got, err := KomariShimConvertServerFixture([]byte(serverJSON))
	require.NoError(t, err)
	require.Equal(t, "7", got.UUID)
	require.Equal(t, "alpha", got.Name)
	require.Equal(t, "linux", got.OS)
	require.Equal(t, int64(1024), got.MemTotal)
	require.Equal(t, 25.5, got.Status.CPU)
	require.Equal(t, int64(512), got.Status.RAM)
	require.Equal(t, int64(44), got.Status.NetTotalUp)
	require.True(t, got.Status.Online)
}
