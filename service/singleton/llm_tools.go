package singleton

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/nezhahq/nezha/model"
)

// LLMRegistry 持有当前可用的 tool 集合；Reload 时构造一次，供 LLMClass.WithTools 调用。
//
// 集中注册的好处：
//   - 添加新工具只动这一个文件，不污染 LLMClass 的逻辑；
//   - 单元测试可以单独拿 Registry 跑，不依赖 ChatModel。
//
// 工具都假定在管理员鉴权后的请求路径里被调用（route 已经是 admin-only），
// 所以工具实现本身不需要再做 user 校验，但仍要遵守最小暴露原则：
// 不要在结果里返回 agent_secret_key / server config 等敏感信息。
type LLMRegistry struct {
	tools    []tool.InvokableTool
	toolInfo []*toolInfo
}

// toolInfo 把 InvokableTool 的名字与 Info 缓存起来，避免每次请求都重新调用 Info。
type toolInfo struct {
	name string
	desc string
}

// NewLLMRegistry 构造内置工具集。
func NewLLMRegistry() *LLMRegistry {
	r := &LLMRegistry{}
	r.registerBuiltin()
	return r
}

// Tools 返回可执行的 InvokableTool 列表（用于手动执行）。
func (r *LLMRegistry) Tools() []tool.InvokableTool {
	return r.tools
}

// ToolInfos 返回 schema 描述（用于传给 ChatModel.WithTools）。
func (r *LLMRegistry) ToolInfos() []*toolInfo {
	out := make([]*toolInfo, len(r.toolInfo))
	copy(out, r.toolInfo)
	return out
}

// registerBuiltin 注册所有内置工具。
func (r *LLMRegistry) registerBuiltin() {
	listServers := r.register(listServersTool())
	_ = listServers // 占位，便于以后扩展时一眼看出当前注册了哪些
}

// register 把工具和它的 Info 缓存起来。重复注册同名工具会 panic，避免拼写错误。
func (r *LLMRegistry) register(t tool.InvokableTool) string {
	if t == nil {
		panic("LLMRegistry.register: nil tool")
	}
	info, err := t.Info(context.Background())
	if err != nil {
		panic(fmt.Sprintf("LLMRegistry.register: info: %v", err))
	}
	for _, existing := range r.toolInfo {
		if existing.name == info.Name {
			panic("LLMRegistry.register: duplicate tool name " + info.Name)
		}
	}
	r.tools = append(r.tools, t)
	r.toolInfo = append(r.toolInfo, &toolInfo{name: info.Name, desc: info.Desc})
	return info.Name
}

// listServersTool 实现 list_servers：返回当前面板已知的所有服务器基础信息。
//
// 输出字段有意保持精简：身份（ID/Name/Tag）、展示（DisplayIndex/HideForGuest）、
// 在线状态、最近活跃时间、硬件概览。不返回 agent_secret_key 或 server 配置等敏感字段。
type listServersInput struct {
	// Keyword 按名称 / Note / Tag 模糊过滤；空字符串返回全部。
	Keyword string `json:"keyword" jsonschema:"description=按名称、备注或分组模糊过滤；留空返回全部"`
	// OnlyOnline true 时只返回在线服务器。
	OnlyOnline bool `json:"only_online" jsonschema:"description=是否仅返回在线服务器"`
	// Limit 限制返回数量；0 表示不限制。
	Limit int `json:"limit" jsonschema:"description=最大返回数量，0 表示不限制"`
}

type serverSummary struct {
	ID            uint64 `json:"id"`
	Name          string `json:"name"`
	Note          string `json:"note,omitempty"`
	DisplayIndex  int    `json:"display_index"`
	HideForGuest  bool   `json:"hide_for_guest"`
	Online        bool   `json:"online"`
	LastActiveISO string `json:"last_active,omitempty"`
	IP            string `json:"ip,omitempty"`
	Platform      string `json:"platform,omitempty"`
	Arch          string `json:"arch,omitempty"`
	Version       string `json:"version,omitempty"`
	CPU           []string `json:"cpu,omitempty"`
	MemTotal      uint64 `json:"mem_total,omitempty"`
	DiskTotal     uint64 `json:"disk_total,omitempty"`
}

type listServersOutput struct {
	Count   int             `json:"count"`
	Servers []serverSummary `json:"servers"`
}

const serverOfflineAfter = 90 * time.Second

func listServersTool() tool.InvokableTool {
	t, err := utils.InferTool[listServersInput, listServersOutput](
		"list_servers",
		"列出当前 Nezha 面板中所有已注册的服务器的基础信息。" +
			"返回字段：ID、Name、Tag、Note、DisplayIndex、HideForGuest、Online、LastActive、" +
			"IP、Platform、Arch、Version、CPU、MemTotal、DiskTotal。" +
			"不做任何修改操作，不会暴露 agent_secret_key 等凭据。",
		func(ctx context.Context, in listServersInput) (listServersOutput, error) {
			if ServerShared == nil {
				return listServersOutput{}, fmt.Errorf("server registry not initialized")
			}

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

			// 按 DisplayIndex 倒序（与前端展示一致），让 LLM 拿到的就是
			// 用户在面板上看到的排序。
			sortServersDesc(out.Servers)

			if in.Limit > 0 && len(out.Servers) > in.Limit {
				out.Servers = out.Servers[:in.Limit]
			}
			out.Count = len(out.Servers)
			return out, nil
		},
	)
	if err != nil {
		panic(fmt.Sprintf("listServersTool: %v", err))
	}
	return t
}

func summarizeServer(s *model.Server, now time.Time) serverSummary {
	out := serverSummary{
		ID:           s.ID,
		Name:         s.Name,
		Note:         s.Note,
		DisplayIndex: s.DisplayIndex,
		HideForGuest: s.HideForGuest,
		Online:       now.Sub(s.LastActive) <= serverOfflineAfter,
	}
	if !s.LastActive.IsZero() {
		out.LastActiveISO = s.LastActive.UTC().Format(time.RFC3339)
	}
	if s.Host != nil {
		out.Platform = s.Host.Platform
		out.Arch = s.Host.Arch
		out.Version = s.Host.Version
		out.CPU = s.Host.CPU
		out.MemTotal = s.Host.MemTotal
		out.DiskTotal = s.Host.DiskTotal
	}
	if s.GeoIP != nil {
		out.IP = s.GeoIP.IP.Join()
	}
	// TODO: 解析 ServerGroupServer 表，把首个 ServerGroup.Name 加到
	// serverSummary 上作为 Tag。当前版本先省略，后续按需扩展。
	return out
}

func matchKeyword(s serverSummary, kw string) bool {
	kwLower := toLower(kw)
	return containsFold(s.Name, kwLower) ||
		containsFold(s.Note, kwLower)
}

func sortServersDesc(servers []serverSummary) {
	// 简单插入排序：服务器数量小（几十~几百台），不必引入 sort 包造成依赖。
	for i := 1; i < len(servers); i++ {
		j := i
		for j > 0 && servers[j-1].DisplayIndex < servers[j].DisplayIndex {
			servers[j-1], servers[j] = servers[j], servers[j-1]
			j--
		}
	}
}

// toLower 与 containsFold 是大小写无关的子串匹配；自己实现避免额外分配。
func toLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func containsFold(haystack, needleLower string) bool {
	if len(needleLower) == 0 {
		return true
	}
	if len(needleLower) > len(haystack) {
		return false
	}
	for i := 0; i+len(needleLower) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needleLower); j++ {
			c := haystack[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needleLower[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
