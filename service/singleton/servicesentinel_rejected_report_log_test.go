package singleton

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
)

// A result that fails canReportServiceResult is entirely agent-controlled: the
// agent picks its size (up to the gRPC receive limit) and how many it sends.
// The rejection used to be logged with %+v, which copied the whole TaskResult
// (data included, every non-printable byte escaped to four characters) into the
// log once per result — on the single worker goroutine that processes every
// tenant's service results. Pin both bounds: the line carries identifiers only,
// and it is written at most once per rejectedReportLogInterval.
func TestRejectedServiceReportIsNotCopiedIntoLog(t *testing.T) {
	var logged bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(originalOutput)

	ss := &ServiceSentinel{services: map[uint64]*model.Service{}}
	servers := &ServerClass{class: class[uint64, *model.Server]{list: map[uint64]*model.Server{}}}
	const rejected = 64
	report := ReportData{
		Reporter: 1,
		Data: &pb.TaskResult{
			Id:   4242424242,
			Type: model.TaskTypeHTTPGet,
			Data: strings.Repeat("\x01", 1<<20),
		},
	}

	start := time.Now()
	for range rejected {
		ss.processReport(report, servers)
	}
	allowedLines := 1 + int(time.Since(start)/rejectedReportLogInterval)

	lines := strings.Count(logged.String(), "Incorrect service monitor report")
	require.GreaterOrEqual(t, lines, 1, "a rejected report must still leave a trace for operators")
	require.LessOrEqual(t, lines, allowedLines, "rejected reports must not be logged once per result")
	require.Less(t, logged.Len(), 512*allowedLines, "the rejected result's data must not reach the log")
	require.Contains(t, logged.String(), "serviceID=4242424242 type=1 reporterID=1")
	require.NotContains(t, logged.String(), `\x01`)
}

// Reports suppressed by the throttle are counted, and the count is carried by
// the next line that is written, so a flood stays visible to operators.
func TestRejectedServiceReportLogCountsSuppressedReports(t *testing.T) {
	var logged bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(originalOutput)

	ss := &ServiceSentinel{}
	report := ReportData{Reporter: 7, Data: &pb.TaskResult{Id: 10, Type: model.TaskTypeTCPPing}}

	ss.logRejectedReport(report)
	require.Contains(t, logged.String(), "serviceID=10 type=3 reporterID=7 suppressedSinceLast=0")

	// Inside the interval of the last written line: counted, not written.
	ss.rejectedReportLoggedAt = time.Now().Add(time.Minute)
	ss.logRejectedReport(report)
	ss.logRejectedReport(report)
	require.Equal(t, 1, strings.Count(logged.String(), "Incorrect service monitor report"))
	require.Equal(t, uint64(2), ss.rejectedReportsSuppressed)

	// Interval elapsed: the next rejection is written and reports the backlog.
	ss.rejectedReportLoggedAt = time.Now().Add(-rejectedReportLogInterval)
	ss.logRejectedReport(report)
	require.Equal(t, 2, strings.Count(logged.String(), "Incorrect service monitor report"))
	require.Contains(t, logged.String(), "serviceID=10 type=3 reporterID=7 suppressedSinceLast=2")
	require.Zero(t, ss.rejectedReportsSuppressed)
}
