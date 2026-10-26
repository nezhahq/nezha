package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAnonymousTransferAuditHasGlobalBudget(t *testing.T) {
	throttle := newTransferAnonAuditThrottle(time.Minute, 5)
	recorded := 0
	for index := 0; index < 80; index++ {
		if throttle.shouldRecord(fmt.Sprintf("192.0.2.%d", index)) {
			recorded++
		}
	}
	require.Equal(t, 60, recorded, "changing source IP must not bypass the process-wide budget")
}

func TestCronPersistenceFailureDoesNotLeaveScheduledCommand(t *testing.T) {
	setupCoverPATFixture(t)
	require.NoError(t, singleton.DB.Callback().Create().Before("gorm:create").Register("reject_cron_persistence", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Cron" {
			tx.AddError(errors.New("simulated cron persistence failure"))
		}
	}))
	before := len(singleton.CronShared.Entries())
	_, err := createCron(ctxAsMemberWithBody(100, model.CronForm{TaskType: model.CronTypeCronTask, Name: "persistence rollback", Scheduler: "@every 1h", Command: "unused-test-command", Servers: []uint64{1}, Cover: model.CronCoverIgnoreAll}))
	require.ErrorContains(t, err, "simulated cron persistence failure")
	require.Len(t, singleton.CronShared.Entries(), before, "failed writes must not leave a live scheduler entry")
}

func TestUploadLengthRejectionPreservesUnusedTransferToken(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	token, _ := mkToken(t, uid, []string{model.ScopeServerWrite}, nil)
	credential, err := mintTransferToken(transferEntry{UserID: uid, TokenID: token.ID, ServerID: 7, Path: "/synthetic-upload", Direction: transferDirUpload, ExpiresAt: time.Now().Add(time.Minute)})
	require.NoError(t, err)
	defer transferEntries.Delete(credential)
	request := httptest.NewRequest(http.MethodPost, "/mcp/upload/"+credential, nil)
	request.ContentLength = -1
	recorder := httptest.NewRecorder()
	newTransferRouter(t).ServeHTTP(recorder, request)
	require.Equal(t, http.StatusLengthRequired, recorder.Code)
	entry, err := consumeTransferToken(credential, transferDirUpload)
	require.NoError(t, err, "retryable request validation must precede atomic token consumption")
	require.NotNil(t, entry)
	_, err = consumeTransferToken(credential, transferDirUpload)
	require.Error(t, err, "successful consumption must remain single-use")
}

func TestReservedUserCannotBeDeleted(t *testing.T) {
	setupCoverPATFixture(t)
	require.NoError(t, singleton.DB.AutoMigrate(&model.Transfer{}))
	context := ctxAsMemberWithBody(999, []uint64{0})
	context.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: 999}, Role: model.RoleAdmin})
	_, err := batchDeleteUser(context)
	require.ErrorContains(t, err, "reserved user")
}
