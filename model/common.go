package model

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/pkg/utils"
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

	UserID uint64 `gorm:"index;default:0" json:"-"`
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

func SearchByIDCtx[S ~[]E, E CommonInterface](c *gin.Context, x S) S {
	switch any(x).(type) {
	case []*Server:
		l := searchByIDCtxServer(c, any(x).([]*Server))
		return any(l).(S)
	default:
		var s S
		for _, idStr := range strings.Split(c.Query("id"), ",") {
			id, err := strconv.ParseUint(idStr, 10, 64)
			if err != nil {
				continue
			}

			if i, ok := slices.BinarySearchFunc(x, id, func(e E, t uint64) int {
				return cmp.Compare(e.GetID(), t)
			}); ok {
				s = append(s, x[i])
			}
		}
		return utils.IfOr(len(s) > 0, s, x)
	}
}

func searchByIDCtxServer(c *gin.Context, x []*Server) []*Server {
	list1, list2 := SplitList(x)

	var clist1, clist2 []*Server
	for _, idStr := range strings.Split(c.Query("id"), ",") {
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			continue
		}

		if i, ok := slices.BinarySearchFunc(list1, id, func(e *Server, t uint64) int {
			return cmp.Compare(e.ID, t)
		}); ok {
			clist1 = append(clist1, list1[i])
		}

		if i, ok := slices.BinarySearchFunc(list2, id, func(e *Server, t uint64) int {
			return cmp.Compare(e.ID, t)
		}); ok {
			clist2 = append(clist2, list2[i])
		}
	}

	l := slices.Concat(clist1, clist2)
	return utils.IfOr(len(l) > 0, l, x)
}

type Response struct {
	Code    int         `json:"code,omitempty"`
	Message string      `json:"message,omitempty"`
	Result  interface{} `json:"result,omitempty"`
}
