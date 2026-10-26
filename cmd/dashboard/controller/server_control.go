package controller

import (
	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func beginServerControl(c *gin.Context, serverID uint64) (func(), error) {
	return serverTaskAdmission(c, serverID)()
}

// Capture request identity before sending: a timed-out Send can outlive Gin's
// request context. Its final admission must not read a recycled *gin.Context.
func serverTaskAdmission(c *gin.Context, serverID uint64) func() (func(), error) {
	user, ok := c.Get(model.CtxKeyAuthorizedUser)
	if !ok || !patAllowsServer(c, serverID) {
		err := singleton.Localizer.ErrorT("permission denied")
		return func() (func(), error) { return nil, err }
	}
	caller := user.(*model.User)
	userID, admin := caller.ID, caller.Role.IsAdmin()
	ctx := c.Request.Context()
	return func() (func(), error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		release, _, err := singleton.BeginServerControl(serverID, userID, admin)
		return release, err
	}
}
