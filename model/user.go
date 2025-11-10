package model

import (
	"time"

	"github.com/gorilla/websocket"
	"github.com/nezhahq/nezha/pkg/utils"
	"gorm.io/gorm"
)

type Role uint8

func (r Role) IsAdmin() bool {
	return r == RoleAdmin
}

const (
	RoleAdmin Role = iota
	RoleMember
)

const DefaultAgentSecretLength = 32

type User struct {
	Common
	Username       string `json:"username,omitempty" gorm:"uniqueIndex"`
	Password       string `json:"password,omitempty" gorm:"type:char(72)"`
	Role           Role   `json:"role,omitempty"`
	AgentSecret    string `json:"agent_secret,omitempty" gorm:"type:char(32)"`
	RejectPassword bool   `json:"reject_password,omitempty"`
}

type UserInfo struct {
	Role        Role
	AgentSecret string
}

func (u *User) BeforeSave(tx *gorm.DB) error {
	if u.AgentSecret != "" {
		return nil
	}

	key, err := utils.GenerateRandomString(DefaultAgentSecretLength)
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
