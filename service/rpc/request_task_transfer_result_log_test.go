package rpc

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
)

// The number of ServerTransferApply results that match no pending transfer is
// chosen by the agent. Each of them used to add a log line of about ten times
// its own size; one stream may now write that line at most once per second.
func TestRequestTaskThrottlesIgnoredTransferResultLog(t *testing.T) {
	srv := &model.Server{
		Common: model.Common{ID: 7, UserID: 100},
		UUID:   "cccccccc-cccc-cccc-cccc-cccccccccccc",
		Name:   "reporter",
	}
	setupApplyConfigAuthzFixture(t, []*model.Server{srv},
		map[uint64]model.UserInfo{100: {Role: model.RoleMember}},
		map[string]uint64{"reporter-secret": 100})

	const ignored = 64
	stream := &requestTaskSecurityStream{
		ctx: metadata.NewIncomingContext(context.Background(), metadata.Pairs(
			"client_secret", "reporter-secret",
			"client_uuid", srv.UUID,
		)),
	}
	for range ignored {
		stream.results = append(stream.results, &pb.TaskResult{Id: 4242424242, Type: model.TaskTypeServerTransferApply})
	}

	var logged bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(originalOutput)

	start := time.Now()
	err := NewNezhaHandler().RequestTask(stream)
	allowedLines := 1 + int(time.Since(start)/time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected RequestTask to finish after test results, got %v", err)
	}

	lines := strings.Count(logged.String(), "ServerTransferApply result ignored")
	if lines < 1 {
		t.Fatal("an ignored ServerTransferApply result must still leave a trace for operators")
	}
	if lines > allowedLines {
		t.Fatalf("%d ignored ServerTransferApply results wrote %d log lines, want at most %d", ignored, lines, allowedLines)
	}
	if !strings.Contains(logged.String(), "clientID=7 reported transferID=4242424242") {
		t.Fatalf("log line must keep identifying the reporter and the transfer, got %q", logged.String())
	}
}
