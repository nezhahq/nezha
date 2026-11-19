package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

type serverEditStateStream struct {
	pb.NezhaService_ReportSystemStateServer
}

func setupServerEditDeleteFixture(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	originalDB := singleton.DB
	originalLocalizer := singleton.Localizer
	originalServer := singleton.ServerShared
	originalTransfer := singleton.ServerTransferShared
	originalDDNS := singleton.DDNSShared
	originalUserInfo := singleton.UserInfoMap

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	// One connection, so every goroutine sees the same in-memory database.
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.Server{}, &model.ServerGroupServer{}, &model.Transfer{}, &model.ServerTransfer{}))

	singleton.DB = db
	singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	singleton.UserLock.Lock()
	singleton.UserInfoMap = map[uint64]model.UserInfo{100: {Role: model.RoleMember}}
	singleton.UserLock.Unlock()
	singleton.ServerShared = singleton.NewServerClass()
	singleton.ServerTransferShared = singleton.NewServerTransferClass()
	singleton.DDNSShared = singleton.NewEmptyDDNSClassForTest()

	t.Cleanup(func() {
		singleton.ServerTransferShared.Stop()
		_ = sqlDB.Close()
		singleton.DB = originalDB
		singleton.Localizer = originalLocalizer
		singleton.ServerShared = originalServer
		singleton.ServerTransferShared = originalTransfer
		singleton.DDNSShared = originalDDNS
		singleton.UserLock.Lock()
		singleton.UserInfoMap = originalUserInfo
		singleton.UserLock.Unlock()
	})
}

func callServerEndpointAsMember(method, path string, body any) commonResponseShape {
	r := gin.New()
	r.Use(gin.Recovery(), func(c *gin.Context) {
		setAuthUser(c, 100, model.RoleMember)
		c.Next()
	})
	r.PATCH("/server/:id", commonHandler(updateServer))
	r.POST("/batch-delete/server", commonHandler(batchDeleteServer))

	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	var resp commonResponseShape
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		resp.Error = "HTTP " + strconv.Itoa(w.Code) + ": " + w.Body.String()
	}
	return resp
}

func editServerAsMember(id uint64) commonResponseShape {
	return callServerEndpointAsMember(http.MethodPatch, "/server/"+strconv.FormatUint(id, 10), model.ServerForm{Name: "renamed"})
}

func deleteServerAsMember(id uint64) commonResponseShape {
	return callServerEndpointAsMember(http.MethodPost, "/batch-delete/server", []uint64{id})
}

// onNextServerSave runs fn once, around the UPDATE of the next edit: before
// its transaction begins, or after it has been committed.
func onNextServerSave(t *testing.T, before bool, fn func()) {
	t.Helper()
	fired := false
	once := func(*gorm.DB) {
		if !fired {
			fired = true
			fn()
		}
	}
	update := singleton.DB.Callback().Update()
	if before {
		require.NoError(t, update.Before("*").Register("test:on_next_server_save", once))
	} else {
		require.NoError(t, update.After("*").Register("test:on_next_server_save", once))
	}
}

func requireServerGone(t *testing.T, id uint64) {
	t.Helper()
	_, registered := singleton.ServerShared.Get(id)
	require.False(t, registered, "the edit registered the deleted server again")
	_, mapped := singleton.ServerShared.UUIDToID("uuid-" + strconv.FormatUint(id, 10))
	require.False(t, mapped)
	var rows int64
	require.NoError(t, singleton.DB.Model(&model.Server{}).Where("id = ?", id).Count(&rows).Error)
	require.Zero(t, rows, "the edit inserted the deleted server again")
}

func TestUpdateServerPublishesEdit(t *testing.T) {
	setupServerEditDeleteFixture(t)
	seedServer(t, 1, 100)
	running, _ := singleton.ServerShared.Get(1)

	resp := editServerAsMember(1)
	require.True(t, resp.Success, resp.Error)

	edited, _ := singleton.ServerShared.Get(1)
	require.Equal(t, "renamed", edited.Name)
	require.Equal(t, running.RuntimeHandle(), edited.RuntimeHandle())
	id, mapped := singleton.ServerShared.UUIDToID("uuid-1")
	require.True(t, mapped)
	require.Equal(t, uint64(1), id)
	var row model.Server
	require.NoError(t, singleton.DB.First(&row, 1).Error)
	require.Equal(t, "renamed", row.Name)
}

func TestUpdateServerDoesNotRestoreServerDeletedBeforeSave(t *testing.T) {
	setupServerEditDeleteFixture(t)
	seedServer(t, 1, 100)
	onNextServerSave(t, true, func() {
		resp := deleteServerAsMember(1)
		require.True(t, resp.Success, resp.Error)
	})

	resp := editServerAsMember(1)
	require.False(t, resp.Success)
	require.Contains(t, resp.Error, "does not exist")
	requireServerGone(t, 1)
}

func TestUpdateServerDoesNotRestoreServerDeletedAfterSave(t *testing.T) {
	setupServerEditDeleteFixture(t)
	seedServer(t, 1, 100)
	onNextServerSave(t, false, func() {
		resp := deleteServerAsMember(1)
		require.True(t, resp.Success, resp.Error)
	})

	resp := editServerAsMember(1)
	require.False(t, resp.Success)
	require.Contains(t, resp.Error, "does not exist")
	requireServerGone(t, 1)
}

// The Agent's state stream holds the server's runtime state while it records
// a report, and an edit that has saved its row waits for it. A deletion that
// arrives meanwhile must not be undone when the edit resumes.
func TestUpdateServerDoesNotRestoreServerDeletedWhileEditWaitsForRuntime(t *testing.T) {
	setupServerEditDeleteFixture(t)
	seedServer(t, 1, 100)
	running, _ := singleton.ServerShared.Get(1)

	reporting := make(chan struct{})
	reported := make(chan struct{})
	lease := running.AttachStateStream(serverEditStateStream{})
	go lease.UpdateStateWithSideEffect(&model.HostState{}, time.Now(), func() error {
		close(reporting)
		<-reported
		return nil
	})
	<-reporting

	saved := make(chan struct{})
	onNextServerSave(t, false, func() { close(saved) })
	edited := make(chan commonResponseShape, 1)
	go func() { edited <- editServerAsMember(1) }()
	<-saved

	deleted := make(chan commonResponseShape, 1)
	go func() { deleted <- deleteServerAsMember(1) }()
	var deletion commonResponseShape
	select {
	case deletion = <-deleted:
	case <-time.After(200 * time.Millisecond):
		// The deletion waits behind the edit; let the edit finish first.
	}
	close(reported)
	<-edited
	if deletion == (commonResponseShape{}) {
		deletion = <-deleted
	}

	require.True(t, deletion.Success, deletion.Error)
	requireServerGone(t, 1)
}
