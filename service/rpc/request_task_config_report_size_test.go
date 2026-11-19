package rpc

import (
	"errors"
	"strings"
	"testing"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

// A config report is returned to the API client as it came, so RequestTask
// must not hand the reader one that is longer than any Agent configuration.
func TestRequestTaskRefusesOversizedConfigReport(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "14141414-1414-1414-1414-141414141414")
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, nil, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})
	server, _ := singleton.ServerShared.Get(reporter.ID)

	report := func(data string, successful bool) any {
		t.Helper()
		runRequestTaskSecurityResult(t, "reporter-secret", reporter.UUID,
			&pb.TaskResult{Type: model.TaskTypeReportConfig, Data: data, Successful: successful})
		select {
		case stored := <-server.ConfigCache:
			return stored
		default:
			t.Fatal("the waiting reader must get an answer")
			return nil
		}
	}

	atLimit := strings.Repeat("<", maxConfigReportBytes)
	if stored, ok := report(atLimit, true).(string); !ok || stored != atLimit {
		t.Fatal("a report at the limit must be stored unchanged")
	}
	if err, ok := report(atLimit, false).(error); !ok || err.Error() != atLimit {
		t.Fatal("a failure reason at the limit must be stored unchanged")
	}
	for _, successful := range []bool{true, false} {
		if err, _ := report(atLimit+"<", successful).(error); !errors.Is(err, ErrConfigReportTooLarge) {
			t.Fatalf("successful=%v: an oversized report must be refused, got %v", successful, err)
		}
	}
}
