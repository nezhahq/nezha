package model

import "time"

type StreamServer struct {
	ID           uint64 `json:"id,omitempty"`
	Name         string `json:"name,omitempty"`
	PublicNote   string `json:"public_note,omitempty"`   // 公开备注，只第一个数据包有值
	DisplayIndex int    `json:"display_index,omitempty"` // 展示排序，越大越靠前

	Host        *Host      `json:"host,omitempty"`
	State       *HostState `json:"state,omitempty"`
	CountryCode string     `json:"country_code,omitempty"`
	LastActive  time.Time  `json:"last_active,omitempty"`
}

type StreamServerData struct {
	Now     int64          `json:"now,omitempty"`
	Online  int            `json:"online,omitempty"`
	Servers []StreamServer `json:"servers,omitempty"`
}

type ServerForm struct {
	Name                string              `json:"name,omitempty"`
	Note                string              `json:"note,omitempty" validate:"optional"`           // 管理员可见备注
	PublicNote          string              `json:"public_note,omitempty" validate:"optional"`    // 公开备注
	DisplayIndex        int                 `json:"display_index,omitempty" default:"0"`          // 展示排序，越大越靠前
	HideForGuest        bool                `json:"hide_for_guest,omitempty" validate:"optional"` // 对游客隐藏
	EnableDDNS          bool                `json:"enable_ddns,omitempty" validate:"optional"`    // 启用DDNS
	DDNSProfiles        []uint64            `json:"ddns_profiles,omitempty" validate:"optional"`  // DDNS配置
	OverrideDDNSDomains map[uint64][]string `json:"override_ddns_domains,omitempty" validate:"optional"`
}

type ServerConfigForm struct {
	Servers []uint64 `json:"servers,omitempty"`
	Config  string   `json:"config,omitempty"`
}

type ServerTaskResponse struct {
	Success []uint64 `json:"success,omitempty" validate:"optional"`
	Failure []uint64 `json:"failure,omitempty" validate:"optional"`
	Offline []uint64 `json:"offline,omitempty" validate:"optional"`
}
