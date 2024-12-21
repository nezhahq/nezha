package model

import (
	"github.com/nezhahq/nezha/pkg/utils"
	"gorm.io/gorm"
)

const (
	RoleAdmin uint8 = iota
	RoleMember
)

type User struct {
	Common
	Username    string `json:"username,omitempty" gorm:"uniqueIndex"`
	Password    string `json:"password,omitempty" gorm:"type:char(72)"`
	Role        uint8  `json:"role,omitempty"`
	AgentSecret string `json:"agent_secret,omitempty" gorm:"type:char(32)"`
}

type UserInfo struct {
	Role        uint8
	_           [3]byte
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
	LoginIP string `json:"login_ip,omitempty"`
}
