package rpc

import (
	"sync/atomic"
	"testing"
	"time"
)

// The Agent decides how often it reports results. Every failed scheduled task
// used to start its own notification goroutine, each holding the result text
// for as long as a slow webhook takes, with no limit.
func TestDispatchTaskNotificationBoundsConcurrentSendsAndThenAppliesBackPressure(t *testing.T) {
	pending := make(chan struct{}, maxPendingTaskNotificationsPerStream)
	release := make(chan struct{})
	var sending, finished atomic.Int32
	slowWebhook := func() {
		sending.Add(1)
		<-release
		finished.Add(1)
	}

	for range maxPendingTaskNotificationsPerStream {
		returned := make(chan struct{})
		go func() {
			dispatchTaskNotification(pending, slowWebhook)
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("a notification within the limit blocked the receive loop")
		}
	}

	overLimitReturned := make(chan struct{})
	go func() {
		dispatchTaskNotification(pending, slowWebhook)
		close(overLimitReturned)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for sending.Load() != maxPendingTaskNotificationsPerStream+1 {
		if time.Now().After(deadline) {
			t.Fatalf("%d notifications started, want every one of them to be sent", sending.Load())
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-overLimitReturned:
		t.Fatal("a notification over the limit returned while its webhook was still pending: nothing bounds the goroutines")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-overLimitReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("the over-limit notification never finished")
	}
	for finished.Load() != maxPendingTaskNotificationsPerStream+1 {
		if time.Now().After(deadline) {
			t.Fatalf("%d notifications finished, want all %d", finished.Load(), maxPendingTaskNotificationsPerStream+1)
		}
		time.Sleep(time.Millisecond)
	}
	// Every slot is free again for the next results.
	for len(pending) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d notification slots still taken after every send finished", len(pending))
		}
		time.Sleep(time.Millisecond)
	}
}
