package controller

import (
	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func beginServerControl(c *gin.Context, serverID uint64) (func(), error) {
	user, ok := c.Get(model.CtxKeyAuthorizedUser)
	if !ok || !patAllowsServer(c, serverID) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}
	caller := user.(*model.User)
	release, _, err := singleton.BeginServerControl(serverID, caller.ID, caller.Role.IsAdmin())
	return release, err
}
