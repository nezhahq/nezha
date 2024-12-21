package model

import (
	"time"

	"github.com/gin-gonic/gin"
)

const (
	CtxKeyAuthorizedUser = "ckau"
	CtxKeyRealIPStr      = "ckri"
)

type CtxKeyRealIP struct{}
type CtxKeyConnectingIP struct{}

type Common struct {
	ID        uint64    `gorm:"primaryKey" json:"id,omitempty"`
	CreatedAt time.Time `gorm:"index;<-:create" json:"created_at,omitempty"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at,omitempty"`
	// Do not use soft deletion
	// DeletedAt gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`

	UserID uint64 `json:"-"`
}

func (c *Common) GetID() uint64 {
	return c.ID
}

func (c *Common) GetUserID() uint64 {
	return c.UserID
}

func (c *Common) HasPermission(ctx *gin.Context) bool {
	auth, ok := ctx.Get(CtxKeyAuthorizedUser)
	if !ok {
		return false
	}

	user := *auth.(*User)
	if user.Role == RoleAdmin {
		return true
	}

	return user.ID == c.UserID
}

type CommonInterface interface {
	GetID() uint64
	GetUserID() uint64
	HasPermission(*gin.Context) bool
}

func FindByUserID[S ~[]E, E CommonInterface](s S, uid uint64) []uint64 {
	var list []uint64
	for _, v := range s {
		if v.GetUserID() == uid {
			list = append(list, v.GetID())
		}
	}

	return list
}

type Response struct {
	Code    int         `json:"code,omitempty"`
	Message string      `json:"message,omitempty"`
	Result  interface{} `json:"result,omitempty"`
}
