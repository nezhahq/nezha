package model

import (
	"time"

	"github.com/gorilla/websocket"
	"github.com/nezhahq/nezha/pkg/utils"
	"gorm.io/gorm"
)

const (
	RoleAdmin uint8 = iota
	RoleMember
)

type User struct {
	Common
	Username       string `json:"username,omitempty" gorm:"uniqueIndex"`
	Password       string `json:"password,omitempty" gorm:"type:char(72)"`
	Role           uint8  `json:"role,omitempty"`
	AgentSecret    string `json:"agent_secret,omitempty" gorm:"type:char(32)"`
	RejectPassword bool   `json:"reject_password,omitempty"`
}

type UserInfo struct {
	Role        uint8
	AgentSecret string
}

func (u *User) BeforeSave(tx *gorm.DB) error {
	if u.AgentSecret != "" {
		return nil
	}

	key, err := utils.GenerateRandomString(32)
	if err != nil {
		return err
	}

	u.AgentSecret = key
	return nil
}

type Profile struct {
	User
	LoginIP    string            `json:"login_ip,omitempty"`
	Oauth2Bind map[string]string `json:"oauth2_bind,omitempty"`
}

type OnlineUser struct {
	UserID      uint64    `json:"user_id,omitempty"`
	ConnectedAt time.Time `json:"connected_at,omitempty"`
	IP          string    `json:"ip,omitempty"`

	Conn *websocket.Conn `json:"-"`
}
