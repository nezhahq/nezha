package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// A number beyond the float32 range is refused by encoding/json while binding
// and binds as an infinity under the go_json build tag; neither build may
// persist it, or the service list stops being valid JSON for every admin.
func TestServiceRejectsOutOfRangeLatency(t *testing.T) {
	setupCoverPATFixture(t)
	r := serviceTypeSecurityRouter()
	service := &model.Service{
		Common:      model.Common{UserID: 100},
		Name:        "valid-service",
		Target:      "example.invalid:443",
		Type:        model.TaskTypeTCPPing,
		Cover:       model.ServiceCoverIgnoreAll,
		SkipServers: map[uint64]bool{1: true},
		Duration:    30,
		MaxLatency:  2000,
	}
	require.NoError(t, singleton.DB.Create(service).Error)

	for _, field := range []string{`"max_latency":1e39`, `"min_latency":-1e39`} {
		body := `{"name":"latency","target":"example.invalid:443","type":3,"cover":1,"skip_servers":{"1":true},"duration":30,` + field + `}`
		for method, path := range map[string]string{
			http.MethodPost:  "/api/v1/service",
			http.MethodPatch: fmt.Sprintf("/api/v1/service/%d", service.ID),
		} {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)

			success, _ := decodeCommonResponseError(t, w.Body.Bytes())
			require.False(t, success, "%s with %s must be rejected", method, field)
		}
	}

	var persisted []model.Service
	require.NoError(t, singleton.DB.Find(&persisted).Error)
	require.Len(t, persisted, 1, "rejected thresholds must not reach persistence")
	require.Zero(t, persisted[0].MinLatency)
	require.Equal(t, float32(2000), persisted[0].MaxLatency)
}
