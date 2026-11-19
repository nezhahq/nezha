package controller

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// createNotification commits the row before it registers it. A notification in
// that state has no owner in the registry, so binding it must fail closed
// instead of relying on the database existence check alone.
func TestTenancy_NotificationGroup_UnregisteredForeignNotificationRejected(t *testing.T) {
	defer setupTenancyTest(t)()
	require.NoError(t, singleton.DB.AutoMigrate(&model.NotificationGroupNotification{}))

	own := model.Notification{Common: model.Common{UserID: 10}, Name: "own-notify", URL: "http://127.0.0.1/"}
	require.NoError(t, singleton.DB.Create(&own).Error)
	singleton.NotificationShared.InsertForTest(&own)
	foreign := model.Notification{Common: model.Common{UserID: 999}, Name: "foreign-notify", URL: "http://127.0.0.1/"}
	require.NoError(t, singleton.DB.Create(&foreign).Error) // committed, not registered yet
	group := model.NotificationGroup{Common: model.Common{UserID: 10}, Name: "own-ng"}
	require.NoError(t, singleton.DB.Create(&group).Error)

	// Padded with duplicates of an own ID, as the racing request was.
	body := map[string]any{"name": "hijacked", "notifications": []uint64{foreign.ID, own.ID, own.ID}}
	_, err := createNotificationGroup(ctxAsMemberWithBody(10, body))
	require.Error(t, err)

	c := ctxAsMemberWithBody(10, body)
	c.Params = gin.Params{{Key: "id", Value: itoa(group.ID)}}
	_, err = updateNotificationGroup(c)
	require.Error(t, err)

	var links int64
	require.NoError(t, singleton.DB.Model(&model.NotificationGroupNotification{}).Count(&links).Error)
	require.Zero(t, links, "a foreign notification must not be linked into the member's group")
	var after model.NotificationGroup
	require.NoError(t, singleton.DB.First(&after, group.ID).Error)
	require.Equal(t, "own-ng", after.Name)

	// Once registered the foreign notification stays forbidden, own ones stay bindable.
	singleton.NotificationShared.InsertForTest(&foreign)
	c = ctxAsMemberWithBody(10, body)
	c.Params = gin.Params{{Key: "id", Value: itoa(group.ID)}}
	_, err = updateNotificationGroup(c)
	require.Error(t, err)

	c = ctxAsMemberWithBody(10, map[string]any{"name": "own-ng", "notifications": []uint64{own.ID, own.ID}})
	c.Params = gin.Params{{Key: "id", Value: itoa(group.ID)}}
	_, err = updateNotificationGroup(c)
	require.NoError(t, err)
	require.NoError(t, singleton.DB.Model(&model.NotificationGroupNotification{}).Count(&links).Error)
	require.EqualValues(t, 1, links)
}
