package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

// fakeTaskStream is the minimum stub of pb.NezhaService_RequestTaskServer
// required to make a server look "online" to forceUpdateServer. Only Send is
// called; we capture its argument so the test can verify the upgrade task is
// NOT dispatched for foreign IDs.
type fakeTaskStream struct {
	pb.NezhaService_RequestTaskServer
	sentTasks []*pb.Task
}

func (f *fakeTaskStream) Send(t *pb.Task) error {
	f.sentTasks = append(f.sentTasks, t)
	return nil
}

// setupServerOwnershipFixture seeds an in-memory DB with alice's server
// (UserID=100, ID=1). The returned stream is wired in so the server is
// "online" — i.e. exercises the path that previously returned permission
// denied for foreign callers (the actual leak channel).
func setupServerOwnershipFixture(t *testing.T) (stream *fakeTaskStream, reset func()) {
	t.Helper()
	if singleton.Localizer == nil {
		singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	}
	originalDB := singleton.DB
	originalShared := singleton.ServerShared

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	assert.NoError(t, err)
	assert.NoError(t, db.AutoMigrate(&model.Server{}))
	assert.NoError(t, db.Create(&model.Server{
		Common: model.Common{ID: 1, UserID: 100},
		Name:   "alice-online",
	}).Error)
	singleton.DB = db
	singleton.ServerShared = singleton.NewServerClass()

	alice, _ := singleton.ServerShared.Get(1)
	stream = &fakeTaskStream{}
	alice.SetTaskStream(stream)

	return stream, func() {
		singleton.DB = originalDB
		singleton.ServerShared = originalShared
	}
}

func runForceUpdate(t *testing.T, callerID uint64, ids []uint64) []byte {
	t.Helper()
	r := gin.New()
	r.Use(func(c *gin.Context) {
		setAuthUser(c, callerID, model.RoleMember)
		c.Next()
	})
	r.POST("/force-update/server", commonHandler(forceUpdateServer))

	body, _ := json.Marshal(ids)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/force-update/server", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w.Body.Bytes()
}

type forceUpdateBody struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Data    struct {
		Offline []uint64 `json:"offline"`
		Success []uint64 `json:"success"`
		Failure []uint64 `json:"failure"`
	} `json:"data"`
}

func decodeForceUpdate(t *testing.T, body []byte) forceUpdateBody {
	t.Helper()
	var resp forceUpdateBody
	assert.NoError(t, json.Unmarshal(body, &resp))
	return resp
}

// Core regression: bob submitting alice's online server ID must NOT produce
// a distinct response from bob submitting an unknown ID. The original code
// returned "permission denied" for the former and a structured success/Offline
// response for the latter — that delta is the enumeration oracle for server
// IDs and online state.
func TestForceUpdateServerOnlineForeignIDIndistinguishableFromUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, reset := setupServerOwnershipFixture(t)
	defer reset()

	const bobID = uint64(200)
	foreignResp := decodeForceUpdate(t, runForceUpdate(t, bobID, []uint64{1}))    // alice's online
	unknownResp := decodeForceUpdate(t, runForceUpdate(t, bobID, []uint64{9999})) // does not exist

	assert.Equal(t, foreignResp.Success, unknownResp.Success,
		"top-level success flag must not differ between foreign-online and unknown IDs")
	assert.Equal(t, foreignResp.Error, unknownResp.Error,
		"error string must not differ — distinct error reveals existence/state of foreign servers")
	assert.Equal(t, foreignResp.Data.Success, unknownResp.Data.Success)
	assert.Equal(t, foreignResp.Data.Failure, unknownResp.Data.Failure)
}

// Submitting a foreign online server must NOT actually trigger the upgrade
// task on it — that would be a write primitive on someone else's machine.
func TestForceUpdateServerForeignOnlineDoesNotDispatchUpgrade(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream, reset := setupServerOwnershipFixture(t)
	defer reset()

	_ = runForceUpdate(t, 200, []uint64{1}) // bob hits alice's online server
	assert.Empty(t, stream.sentTasks,
		"foreign server must not receive the upgrade task even when online")
}

// Sanity: owner submitting their own online server must still get the upgrade
// dispatched and a structured success response — the hardening must not
// regress the legitimate case.
func TestForceUpdateServerOwnerOnlineStillDispatches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream, reset := setupServerOwnershipFixture(t)
	defer reset()

	resp := decodeForceUpdate(t, runForceUpdate(t, 100, []uint64{1})) // alice on her own server
	assert.True(t, resp.Success)
	assert.Equal(t, []uint64{1}, resp.Data.Success)
	assert.Empty(t, resp.Data.Offline)
	assert.Empty(t, resp.Data.Failure)
	assert.Len(t, stream.sentTasks, 1, "owner's own server must receive the upgrade task exactly once")
}

func (*fakeTaskStream) Context() context.Context { return context.Background() }
