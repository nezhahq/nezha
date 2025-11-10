package model

import (
	"cmp"
	"iter"
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
	CtxKeyIsIPMismatch   = "ckipm"
)

const (
	CacheKeyOauth2State = "cko2s::"
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
	return SearchByID(strings.SplitSeq(c.Query("id"), ","), x)
}

func SearchByID[S ~[]E, E CommonInterface](seq iter.Seq[string], x S) S {
	if hasPriorityList[E]() {
		return searchByIDPri(seq, x)
	}

	var s S
	for idStr := range seq {
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			continue
		}

		s = appendBinarySearch(s, x, id)
	}
	return utils.IfOr(len(s) > 0, s, x)
}

func hasPriorityList[T CommonInterface]() bool {
	var class T

	switch any(class).(type) {
	case *Server:
		return true
	default:
		return false
	}
}

type splitter[S ~[]E, E CommonInterface] interface {
	// SplitList should split a sorted list into two separate lists:
	// The first list contains elements with a priority set (DisplayIndex != 0).
	// The second list contains elements without a priority set (DisplayIndex == 0).
	// The original slice is not modified. If no element without a priority is found, it returns nil.
	// Should be safe to use with a nil pointer.
	SplitList(x S) (S, S)
}

func searchByIDPri[S ~[]E, E CommonInterface](seq iter.Seq[string], x S) S {
	var class E
	split, ok := any(class).(splitter[S, E])
	if !ok {
		return x
	}

	plist, list2 := split.SplitList(x)

	var clist1, clist2 S
	for idStr := range seq {
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			continue
		}

		clist1 = appendSearch(clist1, plist, id)
		clist2 = appendBinarySearch(clist2, list2, id)
	}

	l := slices.Concat(clist1, clist2)
	return utils.IfOr(len(l) > 0, l, x)
}

func appendBinarySearch[S ~[]E, E CommonInterface](x, y S, target uint64) S {
	if i, ok := slices.BinarySearchFunc(y, target, func(e E, t uint64) int {
		return cmp.Compare(e.GetID(), t)
	}); ok {
		x = append(x, y[i])
	}
	return x
}

func appendSearch[S ~[]E, E CommonInterface](x, y S, target uint64) S {
	if i := slices.IndexFunc(y, func(e E) bool {
		return e.GetID() == target
	}); i != -1 {
		x = append(x, y[i])
	}

	return x
}
