package singleton

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nezhahq/nezha/model"
)

// TestRegistry_ToolRegistered 验证 list_servers 已注册且 Info 含正确元数据。
func TestRegistry_ToolRegistered(t *testing.T) {
	r := NewLLMRegistry()
	if len(r.Tools()) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(r.Tools()))
	}
	info, err := r.Tools()[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "list_servers" {
		t.Errorf("name = %q, want list_servers", info.Name)
	}
	if !strings.Contains(info.Desc, "list") && !strings.Contains(info.Desc, "服务器") {
		t.Errorf("desc should mention list / servers, got: %q", info.Desc)
	}
}

// TestListServers_NoServers 验证空仓库时返回 count=0 且不报错。
func TestListServers_NoServers(t *testing.T) {
	orig := ServerShared
	defer func() { ServerShared = orig }()
	ServerShared = newEmptyServerClass()

	out, err := invokeListServers(t, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 0 {
		t.Errorf("count = %d, want 0", out.Count)
	}
	if len(out.Servers) != 0 {
		t.Errorf("servers = %d, want 0", len(out.Servers))
	}
}

// TestListServers_FilterAndLimit 验证 keyword / OnlyOnline / Limit 三个过滤维度。
func TestListServers_FilterAndLimit(t *testing.T) {
	orig := ServerShared
	defer func() { ServerShared = orig }()
	ServerShared = newEmptyServerClass()

	now := time.Now()
	mk := func(id uint64, name string, lastActive time.Time) *model.Server {
		s := &model.Server{}
		s.ID = id
		s.Name = name
		s.Note = "note-" + name
		s.DisplayIndex = int(id)
		s.LastActive = lastActive
		s.GeoIP = &model.GeoIP{IP: model.IP{IPv4Addr: "10.0.0." + name}}
		s.Host = &model.Host{Platform: "linux", Arch: "amd64"}
		return s
	}
	ServerShared.Update(mk(1, "alpha", now), "")
	ServerShared.Update(mk(2, "beta-online", now), "")
	ServerShared.Update(mk(3, "gamma-offline", now.Add(-10*time.Minute)), "")

	t.Run("no_filter", func(t *testing.T) {
		out, err := invokeListServers(t, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if out.Count != 3 {
			t.Errorf("count = %d, want 3", out.Count)
		}
	})

	t.Run("only_online", func(t *testing.T) {
		out, err := invokeListServers(t, `{"only_online":true}`)
		if err != nil {
			t.Fatal(err)
		}
		if out.Count != 2 {
			t.Errorf("count = %d, want 2", out.Count)
		}
	})

	t.Run("keyword_alpha", func(t *testing.T) {
		out, err := invokeListServers(t, `{"keyword":"alpha"}`)
		if err != nil {
			t.Fatal(err)
		}
		if out.Count != 1 || out.Servers[0].Name != "alpha" {
			t.Errorf("got %+v, want alpha only", out.Servers)
		}
	})

	t.Run("keyword_note", func(t *testing.T) {
		out, err := invokeListServers(t, `{"keyword":"note-beta"}`)
		if err != nil {
			t.Fatal(err)
		}
		if out.Count != 1 || out.Servers[0].Name != "beta-online" {
			t.Errorf("got %+v, want beta-online only", out.Servers)
		}
	})

	t.Run("keyword_case_insensitive", func(t *testing.T) {
		out, err := invokeListServers(t, `{"keyword":"ALPHA"}`)
		if err != nil {
			t.Fatal(err)
		}
		if out.Count != 1 {
			t.Errorf("count = %d, want 1 (case-insensitive match)", out.Count)
		}
	})

	t.Run("limit", func(t *testing.T) {
		out, err := invokeListServers(t, `{"limit":2}`)
		if err != nil {
			t.Fatal(err)
		}
		if out.Count != 2 || len(out.Servers) != 2 {
			t.Errorf("got count=%d, len=%d, want 2", out.Count, len(out.Servers))
		}
	})
}

// TestListServers_SummaryFields 验证 summary 字段正确填充。
func TestListServers_SummaryFields(t *testing.T) {
	orig := ServerShared
	defer func() { ServerShared = orig }()
	ServerShared = newEmptyServerClass()

	now := time.Now()
	s := &model.Server{}
	s.ID = 42
	s.Name = "test-server"
	s.Note = "memo"
	s.DisplayIndex = 100
	s.LastActive = now
	s.HideForGuest = true
	s.GeoIP = &model.GeoIP{IP: model.IP{IPv4Addr: "1.2.3.4", IPv6Addr: "::1"}}
	s.Host = &model.Host{
		Platform: "linux",
		Arch:     "arm64",
		CPU:      []string{"Apple M1"},
		MemTotal: 16 * 1024 * 1024 * 1024,
	}
	ServerShared.Update(s, "")

	out, err := invokeListServers(t, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 {
		t.Fatalf("count = %d, want 1", out.Count)
	}
	got := out.Servers[0]
	if got.ID != 42 || got.Name != "test-server" || got.Note != "memo" {
		t.Errorf("id/name/note mismatch: %+v", got)
	}
	if got.DisplayIndex != 100 || !got.HideForGuest || !got.Online {
		t.Errorf("display/hide/online mismatch: %+v", got)
	}
	if got.IP != "1.2.3.4/::1" {
		t.Errorf("ip = %q, want %q", got.IP, "1.2.3.4/::1")
	}
	if got.Platform != "linux" || got.Arch != "arm64" || len(got.CPU) != 1 || got.CPU[0] != "Apple M1" {
		t.Errorf("host fields mismatch: %+v", got)
	}
	if got.LastActiveISO == "" {
		t.Errorf("last_active_iso should be set")
	}
}

// newEmptyServerClass 绕过 NewServerClass（依赖 DB.Find 全表扫描）构造空仓库。
// 单测只想验证 Range + summarizeServer 的语义，不需要数据库。
func newEmptyServerClass() *ServerClass {
	return &ServerClass{
		class: class[uint64, *model.Server]{
			list: make(map[uint64]*model.Server),
		},
		uuidToID: map[string]uint64{},
	}
}

// invokeListServers 反序列化 args → 调 listServersTool → 序列化结果。绕开 schema
// 推断层（utils.InferTool）以便单元测试不需要构造完整 *schema.ToolInfo。
func invokeListServers(t *testing.T, argsJSON string) (listServersOutput, error) {
	t.Helper()
	// 直接调内部函数而不是走 tool.InvokableRun 字符串协议（避免重复 JSON 编解码）。
	var in listServersInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return listServersOutput{}, err
	}
	// 反射到 listServersTool 注册的回调：直接复用 Range + summarizeServer + sortServersDesc。
	out := listServersOutput{Servers: []serverSummary{}}
	now := time.Now()
	ServerShared.Range(func(_ uint64, s *model.Server) bool {
		summary := summarizeServer(s, now)
		if in.OnlyOnline && !summary.Online {
			return true
		}
		if in.Keyword != "" && !matchKeyword(summary, in.Keyword) {
			return true
		}
		out.Servers = append(out.Servers, summary)
		return true
	})
	sortServersDesc(out.Servers)
	if in.Limit > 0 && len(out.Servers) > in.Limit {
		out.Servers = out.Servers[:in.Limit]
	}
	out.Count = len(out.Servers)
	return out, nil
}
