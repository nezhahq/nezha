package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func taskDataLimitRouter() *gin.Engine {
	r := serviceTypeSecurityRouter()
	r.POST("/api/v1/cron", commonHandler(createCron))
	r.PATCH("/api/v1/cron/:id", commonHandler(updateCron))
	return r
}

func taskDataLimitRequest(t *testing.T, r *gin.Engine, method, path string, form any) (bool, string) {
	t.Helper()
	body, err := json.Marshal(form)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return decodeCommonResponseError(t, w.Body.Bytes())
}

func TestServiceAPIRejectsOversizedTarget(t *testing.T) {
	setupCoverPATFixture(t)
	r := taskDataLimitRouter()
	form := model.ServiceForm{
		Name:        "service-target-limit",
		Target:      strings.Repeat("4", model.MaxServiceTargetBytes),
		Type:        model.TaskTypeTCPPing,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
		Duration:    30,
	}

	success, errMsg := taskDataLimitRequest(t, r, http.MethodPost, "/api/v1/service", form)
	require.True(t, success, "the largest allowed target must remain accepted: %s", errMsg)
	var service model.Service
	require.NoError(t, singleton.DB.First(&service).Error)

	// Surrounding whitespace is trimmed before the target is stored and sent.
	form.Target = " " + form.Target + "4 "
	path := fmt.Sprintf("/api/v1/service/%d", service.ID)
	for method, path := range map[string]string{http.MethodPost: "/api/v1/service", http.MethodPatch: path} {
		success, errMsg = taskDataLimitRequest(t, r, method, path, form)
		require.False(t, success, "%s must reject an oversized target", method)
		require.Contains(t, errMsg, "service target exceeds")
	}

	var count int64
	require.NoError(t, singleton.DB.Model(&model.Service{}).Count(&count).Error)
	require.EqualValues(t, 1, count, "a rejected target must not reach persistence")
	var persisted model.Service
	require.NoError(t, singleton.DB.First(&persisted, service.ID).Error)
	require.Len(t, persisted.Target, model.MaxServiceTargetBytes)
}

func TestCronAPIRejectsOversizedCommand(t *testing.T) {
	setupCoverPATFixture(t)
	r := taskDataLimitRouter()
	form := model.CronForm{
		TaskType:  model.CronTypeCronTask,
		Name:      "cron-command-limit",
		Scheduler: "0 0 0 1 1 *",
		Command:   strings.Repeat("#", model.MaxCronCommandBytes),
		Servers:   []uint64{1},
		Cover:     model.CronCoverIgnoreAll,
	}

	success, errMsg := taskDataLimitRequest(t, r, http.MethodPost, "/api/v1/cron", form)
	require.True(t, success, "the largest allowed command must remain accepted: %s", errMsg)
	var cron model.Cron
	require.NoError(t, singleton.DB.First(&cron).Error)

	form.Command += "#"
	path := fmt.Sprintf("/api/v1/cron/%d", cron.ID)
	for method, path := range map[string]string{http.MethodPost: "/api/v1/cron", http.MethodPatch: path} {
		success, errMsg = taskDataLimitRequest(t, r, method, path, form)
		require.False(t, success, "%s must reject an oversized command", method)
		require.Contains(t, errMsg, "task command exceeds")
	}

	var count int64
	require.NoError(t, singleton.DB.Model(&model.Cron{}).Count(&count).Error)
	require.EqualValues(t, 1, count, "a rejected command must not reach persistence")
	var persisted model.Cron
	require.NoError(t, singleton.DB.First(&persisted, cron.ID).Error)
	require.Len(t, persisted.Command, model.MaxCronCommandBytes)
}
