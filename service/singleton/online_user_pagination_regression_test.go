package singleton

import (
	"math"
	"testing"

	"github.com/nezhahq/nezha/model"
	"github.com/stretchr/testify/require"
)

func TestSecurityBoundaryOnlinePaginationOverflow(t *testing.T) {
	original := OnlineUserMap
	OnlineUserMap = map[string]*model.OnlineUser{"first": {}, "second": {}}
	t.Cleanup(func() { OnlineUserMap = original })
	require.Len(t, GetOnlineUsers(1, 1), 1)
	require.Len(t, GetOnlineUsers(math.MaxInt, 0), 2)
	require.Len(t, GetOnlineUsers(math.MaxInt, 1), 1)
}
