package rpc

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

func TestTruncateTaskResultTextKeepsShortOutputAndCutsOnRuneBoundary(t *testing.T) {
	atLimit := strings.Repeat("a", maxTaskResultTextBytes)
	if got := truncateTaskResultText(atLimit); got != atLimit {
		t.Fatal("output at the limit must be kept unchanged")
	}

	// "界" is three bytes. With two ASCII bytes in front the limit falls inside a
	// character (the limit minus two is not a multiple of three), so a plain byte
	// cut would split it. One byte in front would not: the limit minus one is.
	const asciiPrefix = "ab"
	if (maxTaskResultTextBytes-len(asciiPrefix))%len("界") == 0 {
		t.Fatal("test setup: the limit must fall inside a multi-byte character")
	}
	oversized := asciiPrefix + strings.Repeat("界", maxTaskResultTextBytes)
	got := truncateTaskResultText(oversized)
	if !utf8.ValidString(got) {
		t.Fatal("truncated output is not valid UTF-8")
	}
	kept := strings.TrimSuffix(got, taskResultTruncatedSuffix)
	if len(kept) >= maxTaskResultTextBytes || len(kept) < maxTaskResultTextBytes-len("界") {
		t.Fatalf("kept %d bytes, want the last whole character below the %d byte limit", len(kept), maxTaskResultTextBytes)
	}
	if !strings.HasSuffix(got, taskResultTruncatedSuffix) {
		t.Fatal("truncated output does not say it was truncated")
	}
	if len(got) > maxTaskResultTextBytes+len(taskResultTruncatedSuffix) {
		t.Fatalf("truncated output is %d bytes, limit is %d", len(got), maxTaskResultTextBytes)
	}
}

// The Agent decides how long a monitor result is. Megabytes of it used to be
// kept as the monitor's last result and written into its history.
func TestRequestTaskBoundsServiceMonitorResultText(t *testing.T) {
	reporter := requestTaskSecurityServer(7, 200, "15151515-1515-1515-1515-151515151515")
	setupRequestTaskSecurityFixture(t, []*model.Server{reporter}, nil, map[uint64]model.UserInfo{
		200: {Role: model.RoleMember},
	}, map[string]uint64{"reporter-secret": 200})
	previousSentinel := singleton.ServiceSentinelShared
	t.Cleanup(func() { singleton.ServiceSentinelShared = previousSentinel })
	dispatched := make(chan singleton.ReportData, 1)
	singleton.ServiceSentinelShared = singleton.NewServiceSentinelForReportCapture(dispatched)

	runRequestTaskSecurityResult(t, "reporter-secret", reporter.UUID,
		&pb.TaskResult{Type: model.TaskTypeHTTPGet, Id: 1, Data: strings.Repeat("x", 4*1024*1024)})

	select {
	case report := <-dispatched:
		if got := len(report.Data.GetData()); got > maxTaskResultTextBytes+len(taskResultTruncatedSuffix) {
			t.Fatalf("monitor result reached the sentinel with %d bytes, limit is %d", got, maxTaskResultTextBytes)
		}
	default:
		t.Fatal("the monitor result was not dispatched")
	}
}
