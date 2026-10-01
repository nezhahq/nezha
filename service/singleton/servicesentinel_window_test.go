package singleton

import (
	"testing"
	"time"

	pb "github.com/nezhahq/nezha/proto"
)

func TestServiceWindowCountsFirstFailureAsDown(t *testing.T) {
	status := new(serviceTaskStatus)
	status.appendResult(time.Unix(1000, 0), 1, &pb.TaskResult{Successful: false})
	data := status.responseData()
	if data.Down != 1 || data.Up != 0 || GetStatusCode(uint64(0)) != StatusDown {
		t.Fatalf("first failed report = %+v, status %d; want Down", data, GetStatusCode(uint64(0)))
	}
}

func TestServiceWindowExpiresOldReportsAndDoesNotResetAtThirty(t *testing.T) {
	status := new(serviceTaskStatus)
	start := time.Unix(1000, 0)
	for i := 0; i < 30; i++ {
		status.appendResult(start.Add(time.Duration(i)*30*time.Second), 1, &pb.TaskResult{Successful: true})
	}
	if data := status.responseData(); data.Up != 30 {
		t.Fatalf("30th report reset window: %+v", data)
	}
	status.appendResult(start.Add(29*30*time.Second+time.Second), 2, &pb.TaskResult{Successful: false})
	if data := status.responseData(); data.Up != 30 || data.Down != 1 {
		t.Fatalf("31st report corrupted window: %+v", data)
	}
	status.appendResult(start.Add(31*time.Minute), 1, &pb.TaskResult{Successful: false})
	if data := status.responseData(); data.Up != 0 || data.Down != 1 {
		t.Fatalf("expired reports still influence status: %+v", data)
	}
}

func TestServiceWindowDeduplicatesFloodWithinBucket(t *testing.T) {
	status := new(serviceTaskStatus)
	now := time.Unix(1000, 0)
	for i := 0; i < 10000; i++ {
		status.appendResult(now, 1, &pb.TaskResult{Successful: false, Data: "ignored payload"})
	}
	if len(status.result) != 1 || len(status.result[0].reporters) != 1 {
		t.Fatalf("flood grew live window: buckets=%d", len(status.result))
	}
	if data := status.responseData(); data.Down != 1 {
		t.Fatalf("duplicate reporter skewed status: %+v", data)
	}
	if status.history.Down != 10000 {
		t.Fatalf("history lost raw reports: %+v", status.history)
	}
}

func TestServiceWindowDoesNotEraseFailureWithLaterSuccess(t *testing.T) {
	status := new(serviceTaskStatus)
	now := time.Unix(1000, 0)
	status.appendResult(now, 1, &pb.TaskResult{Successful: false})
	status.appendResult(now.Add(time.Second), 1, &pb.TaskResult{Successful: true, Delay: 20})
	if data := status.responseData(); data.Down != 1 || data.Up != 1 {
		t.Fatalf("later success erased failure in same bucket: %+v", data)
	}
}
