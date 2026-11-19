package model

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	pb "github.com/nezhahq/nezha/proto"
)

type runtimeOwnershipStream struct{}

func (runtimeOwnershipStream) Send(*pb.Receipt) error       { return nil }
func (runtimeOwnershipStream) Recv() (*pb.State, error)     { return nil, context.Canceled }
func (runtimeOwnershipStream) SetHeader(metadata.MD) error  { return nil }
func (runtimeOwnershipStream) SendHeader(metadata.MD) error { return nil }
func (runtimeOwnershipStream) SetTrailer(metadata.MD)       {}
func (runtimeOwnershipStream) Context() context.Context     { return context.Background() }
func (runtimeOwnershipStream) SendMsg(any) error            { return nil }
func (runtimeOwnershipStream) RecvMsg(any) error            { return nil }

func TestServerRuntimeOwnership_replacementAdoptsHolderBeforeFirstAttach(t *testing.T) {
	old := &Server{State: &HostState{Uptime: 1}, Host: &Host{BootTime: 10}}
	newServer := &Server{}
	var lease StateStreamLease
	started := make(chan struct{})
	var waitGroup sync.WaitGroup
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		close(started)
		lease = old.AttachStateStream(runtimeOwnershipStream{})
	}()
	<-started
	newServer.CopyFromRunningServer(old)
	waitGroup.Wait()

	require.True(t, newServer.UpdateStateIfCurrent(lease, &HostState{Uptime: 2}, time.Unix(2, 0)))
	snapshot := newServer.RuntimeSnapshot()
	require.Equal(t, uint64(2), snapshot.State.Uptime)
	require.Equal(t, time.Unix(2, 0), snapshot.LastActive)
	require.False(t, old.ClearStateStreamIfCurrent(lease))
}

func TestServerRuntimeOwnership_oldLeaseMutatesCanonicalAfterReplacement(t *testing.T) {
	old := &Server{}
	InitServer(old)
	lease := old.AttachStateStream(runtimeOwnershipStream{})
	newServer := &Server{}
	newServer.CopyFromRunningServer(old)

	require.True(t, newServer.UpdateStateIfCurrent(lease, &HostState{Uptime: 7}, time.Unix(7, 0)))
	snapshot := newServer.RuntimeSnapshot()
	require.Equal(t, uint64(7), snapshot.State.Uptime)
	require.Equal(t, time.Unix(7, 0), snapshot.LastActive)
	require.False(t, old.ClearStateStreamIfCurrent(lease))
	require.True(t, newServer.ClearStateStreamIfCurrent(lease))
	require.True(t, newServer.RuntimeSnapshot().LastActive.IsZero())
}

func TestServerRuntimeOwnership_leaseMutatesCanonicalWithoutReceiver(t *testing.T) {
	// Given
	old := &Server{}
	InitServer(old)
	lease := old.AttachStateStream(runtimeOwnershipStream{})
	canonical := &Server{}
	canonical.CopyFromRunningServer(old)

	// When
	accepted := lease.UpdateState(&HostState{Uptime: 19}, time.Unix(19, 0))

	// Then
	require.True(t, accepted)
	require.Equal(t, uint64(19), canonical.RuntimeSnapshot().State.Uptime)
	require.Equal(t, time.Unix(19, 0), canonical.RuntimeSnapshot().LastActive)
}

func TestServerRuntimeOwnership_oldReceiverMutatorsCannotChangeCanonical(t *testing.T) {
	// Given
	old := &Server{}
	InitServer(old)
	lease := old.AttachStateStream(runtimeOwnershipStream{})
	canonical := &Server{}
	canonical.CopyFromRunningServer(old)

	// When
	hostChanged := old.SetHost(&Host{Version: "stale"})
	snapshotChanged := old.SetTransferSnapshots(91, 92)
	inbound, outbound, deltaIn, deltaOut := old.TransferDeltaAndAdvance()

	// Then
	require.False(t, hostChanged)
	require.False(t, snapshotChanged)
	require.Equal(t, uint64(0), inbound)
	require.Equal(t, uint64(0), outbound)
	require.Equal(t, uint64(0), deltaIn)
	require.Equal(t, uint64(0), deltaOut)
	require.Empty(t, canonical.RuntimeSnapshot().Host.Version)
	require.Equal(t, uint64(0), canonical.RuntimeSnapshot().PrevTransferInSnapshot)
	require.Equal(t, uint64(0), canonical.RuntimeSnapshot().PrevTransferOutSnapshot)
	require.True(t, lease.UpdateState(&HostState{NetInTransfer: 10, NetOutTransfer: 20}, time.Unix(20, 0)))
}

func TestServerRuntimeOwnership_copyFallbackPreservesHost(t *testing.T) {
	// Given
	old := &Server{Host: &Host{Version: "fallback"}, State: &HostState{Uptime: 4}, LastActive: time.Unix(4, 0), PrevTransferInSnapshot: 5, PrevTransferOutSnapshot: 6}
	canonical := &Server{}

	// When
	canonical.CopyFromRunningServer(old)

	// Then
	snapshot := canonical.RuntimeSnapshot()
	require.Equal(t, "fallback", snapshot.Host.Version)
	require.Equal(t, uint64(4), snapshot.State.Uptime)
	require.Equal(t, time.Unix(4, 0), snapshot.LastActive)
	require.Equal(t, uint64(5), snapshot.PrevTransferInSnapshot)
	require.Equal(t, uint64(6), snapshot.PrevTransferOutSnapshot)
}

func TestServerRuntimeSnapshot_isSafeDuringStateUpdates(t *testing.T) {
	server := &Server{}
	InitServer(server)
	lease := server.AttachStateStream(runtimeOwnershipStream{})
	var waitGroup sync.WaitGroup
	waitGroup.Add(2)
	go func() {
		defer waitGroup.Done()
		for index := uint64(1); index <= 500; index++ {
			server.UpdateStateIfCurrent(lease, &HostState{Uptime: index, GPU: []float64{float64(index)}}, time.Unix(int64(index), 0))
		}
	}()
	go func() {
		defer waitGroup.Done()
		for index := 0; index < 500; index++ {
			snapshot := server.RuntimeSnapshot()
			require.NotNil(t, snapshot.State)
			if snapshot.State.Uptime > 0 {
				require.Len(t, snapshot.State.GPU, 1)
			}
		}
	}()
	waitGroup.Wait()
}

func TestServerRuntimeOwnership_restartHostReportUsesCurrentCanonicalOnce(t *testing.T) {
	// Given
	old := &Server{}
	InitServer(old)
	lease := old.AttachStateStream(runtimeOwnershipStream{})
	require.True(t, lease.UpdateState(&HostState{NetInTransfer: 140, NetOutTransfer: 90}, time.Unix(10, 0)))
	require.True(t, old.SetTransferSnapshots(100, 70))
	middle := &Server{}
	middle.CopyFromRunningServer(old)
	current := &Server{}
	current.CopyFromRunningServer(middle)

	// When
	result, err := old.RuntimeHandle().ApplyHostReport(&Host{BootTime: 20}, time.Unix(20, 0), nil)

	// Then
	require.NoError(t, err)
	require.True(t, result.Applied)
	require.True(t, result.Restart)
	require.Equal(t, current.ID, result.ServerID)
	require.Equal(t, uint64(40), result.Transfer.In)
	require.Equal(t, uint64(20), result.Transfer.Out)
	require.Equal(t, uint64(0), current.RuntimeSnapshot().PrevTransferInSnapshot)
	require.Equal(t, uint64(0), current.RuntimeSnapshot().PrevTransferOutSnapshot)
	require.Equal(t, uint64(20), current.RuntimeSnapshot().Host.BootTime)

	secondResult, secondErr := old.RuntimeHandle().ApplyHostReport(&Host{BootTime: 20}, time.Unix(21, 0), nil)
	require.NoError(t, secondErr)
	require.True(t, secondResult.Applied)
	require.True(t, secondResult.Equal)
	require.Zero(t, secondResult.Transfer)
}

func TestServerRuntimeOwnership_hostReportPersistenceFailurePreservesRuntime(t *testing.T) {
	// Given
	old := &Server{}
	InitServer(old)
	lease := old.AttachStateStream(runtimeOwnershipStream{})
	require.True(t, lease.UpdateState(&HostState{NetInTransfer: 140, NetOutTransfer: 90}, time.Unix(10, 0)))
	require.True(t, old.SetTransferSnapshots(100, 70))
	current := &Server{}
	current.CopyFromRunningServer(old)
	handle := old.RuntimeHandle()
	before := current.RuntimeSnapshot()

	// When
	_, err := handle.ApplyHostReport(&Host{BootTime: 20}, time.Unix(20, 0), func(Transfer) error {
		return context.Canceled
	})

	// Then
	require.ErrorIs(t, err, context.Canceled)
	after := current.RuntimeSnapshot()
	require.Equal(t, before.Host, after.Host)
	require.Equal(t, before.State, after.State)
	require.Equal(t, before.LastActive, after.LastActive)
	require.Equal(t, before.PrevTransferInSnapshot, after.PrevTransferInSnapshot)
	require.Equal(t, before.PrevTransferOutSnapshot, after.PrevTransferOutSnapshot)
}

func TestServerRuntimeOwnership_hostReportRetryPersistsExactlyOnce(t *testing.T) {
	// Given
	server := &Server{Common: Common{ID: 41}, UUID: "server-41"}
	InitServer(server)
	lease := server.AttachStateStream(runtimeOwnershipStream{})
	require.True(t, lease.UpdateState(&HostState{NetInTransfer: 20, NetOutTransfer: 30}, time.Unix(10, 0)))
	require.True(t, server.SetTransferSnapshots(5, 10))
	callbackCalls := 0
	callback := func(transfer Transfer) error {
		callbackCalls++
		if callbackCalls == 1 {
			return context.Canceled
		}
		return nil
	}
	handle := server.RuntimeHandle()

	// When
	first, firstErr := handle.ApplyHostReport(&Host{BootTime: 20}, time.Unix(20, 0), callback)
	second, secondErr := handle.ApplyHostReport(&Host{BootTime: 20}, time.Unix(20, 0), callback)
	third, thirdErr := handle.ApplyHostReport(&Host{BootTime: 20}, time.Unix(21, 0), callback)

	// Then
	require.ErrorIs(t, firstErr, context.Canceled)
	require.NoError(t, secondErr)
	require.NoError(t, thirdErr)
	require.Equal(t, 2, callbackCalls)
	require.Equal(t, uint64(15), second.Transfer.In)
	require.Equal(t, uint64(20), second.Transfer.Out)
	require.True(t, third.Equal)
	require.Zero(t, third.Transfer)
	_ = first
}

func TestServerRuntimeOwnership_hostReportClassifiesLowerAndEqualWithoutRestart(t *testing.T) {
	// Given
	server := &Server{Common: Common{ID: 42}, UUID: "server-42"}
	InitServer(server)
	require.True(t, server.SetHost(&Host{BootTime: 20, Version: "old"}))
	lease := server.AttachStateStream(runtimeOwnershipStream{})
	require.True(t, lease.UpdateState(&HostState{Uptime: 7, NetInTransfer: 30}, time.Unix(7, 0)))
	require.True(t, server.SetTransferSnapshots(12, 0))
	persistCalls := 0
	persist := func(Transfer) error { persistCalls++; return nil }
	handle := server.RuntimeHandle()

	// When
	lower, lowerErr := handle.ApplyHostReport(&Host{BootTime: 19, Version: "stale"}, time.Unix(8, 0), persist)
	equal, equalErr := handle.ApplyHostReport(&Host{BootTime: 20, Version: "new"}, time.Unix(9, 0), persist)

	// Then
	require.NoError(t, lowerErr)
	require.True(t, lower.Stale)
	require.NoError(t, equalErr)
	require.True(t, equal.Equal)
	require.Zero(t, persistCalls)
	snapshot := server.RuntimeSnapshot()
	require.Equal(t, "new", snapshot.Host.Version)
	require.Equal(t, uint64(7), snapshot.State.Uptime)
	require.Equal(t, time.Unix(7, 0), snapshot.LastActive)
	require.Equal(t, uint64(12), snapshot.PrevTransferInSnapshot)
}

func TestServerRuntimeOwnership_hostReportReturnsLatestCanonicalIdentity(t *testing.T) {
	// Given
	old := &Server{Common: Common{ID: 11}, UUID: "old"}
	InitServer(old)
	middle := &Server{Common: Common{ID: 22}, UUID: "middle"}
	middle.CopyFromRunningServer(old)
	current := &Server{Common: Common{ID: 33}, UUID: "current"}
	current.CopyFromRunningServer(middle)

	// When
	result, err := old.RuntimeHandle().ApplyHostReport(&Host{BootTime: 1}, time.Unix(1, 0), nil)

	// Then
	require.NoError(t, err)
	require.True(t, result.Applied)
	require.Equal(t, current.ID, result.ServerID)
	require.Equal(t, current.UUID, result.UUID)
}

func TestServerRuntimeOwnership_transferAndRestartDoNotDuplicateWhenTransferRunsFirst(t *testing.T) {
	// Given
	server := &Server{Common: Common{ID: 51}, UUID: "server-51"}
	InitServer(server)
	lease := server.AttachStateStream(runtimeOwnershipStream{})
	require.True(t, lease.UpdateState(&HostState{NetInTransfer: 100, NetOutTransfer: 200}, time.Unix(10, 0)))
	require.True(t, server.SetTransferSnapshots(40, 80))
	handle := server.RuntimeHandle()
	holder := handle.holder
	holder.mu.Lock()
	hourlyDone := make(chan struct{})
	go func() {
		server.TransferDeltaAndAdvance()
		close(hourlyDone)
	}()
	holder.mu.Unlock()
	<-hourlyDone

	// When
	records := 0
	result, err := handle.ApplyHostReport(&Host{BootTime: 20}, time.Unix(20, 0), func(Transfer) error { records++; return nil })

	// Then
	require.NoError(t, err)
	require.Equal(t, 1, records)
	require.Equal(t, uint64(0), result.Transfer.In)
	require.Equal(t, uint64(0), result.Transfer.Out)
	require.Equal(t, uint64(51), result.ServerID)
	require.Equal(t, uint64(0), server.RuntimeSnapshot().PrevTransferInSnapshot)
}

func TestServerRuntimeOwnership_restartAndTransferDoNotDuplicateWhenRestartRunsFirst(t *testing.T) {
	// Given
	server := &Server{Common: Common{ID: 52}, UUID: "server-52"}
	InitServer(server)
	lease := server.AttachStateStream(runtimeOwnershipStream{})
	require.True(t, lease.UpdateState(&HostState{NetInTransfer: 100, NetOutTransfer: 200}, time.Unix(10, 0)))
	require.True(t, server.SetTransferSnapshots(40, 80))
	handle := server.RuntimeHandle()
	result, err := handle.ApplyHostReport(&Host{BootTime: 20}, time.Unix(20, 0), func(Transfer) error { return nil })
	require.NoError(t, err)

	// When
	inbound, outbound, deltaIn, deltaOut := server.TransferDeltaAndAdvance()

	// Then
	require.Equal(t, uint64(60), result.Transfer.In)
	require.Equal(t, uint64(120), result.Transfer.Out)
	require.Equal(t, uint64(0), inbound)
	require.Equal(t, uint64(0), outbound)
	require.Equal(t, uint64(0), deltaIn)
	require.Equal(t, uint64(0), deltaOut)
}

func TestServerRuntimeOwnership_failedRestartAllowsHourlyRecordThenRetry(t *testing.T) {
	// Given
	server := &Server{Common: Common{ID: 53}, UUID: "server-53"}
	InitServer(server)
	lease := server.AttachStateStream(runtimeOwnershipStream{})
	require.True(t, lease.UpdateState(&HostState{NetInTransfer: 90, NetOutTransfer: 110}, time.Unix(10, 0)))
	require.True(t, server.SetTransferSnapshots(30, 50))
	handle := server.RuntimeHandle()
	_, err := handle.ApplyHostReport(&Host{BootTime: 20}, time.Unix(20, 0), func(Transfer) error { return context.Canceled })
	require.ErrorIs(t, err, context.Canceled)

	// When
	_, _, hourlyIn, hourlyOut := server.TransferDeltaAndAdvance()
	records := 0
	result, retryErr := handle.ApplyHostReport(&Host{BootTime: 20}, time.Unix(20, 0), func(Transfer) error { records++; return nil })

	// Then
	require.NoError(t, retryErr)
	require.Equal(t, uint64(60), hourlyIn)
	require.Equal(t, uint64(60), hourlyOut)
	require.Equal(t, 1, records)
	require.Equal(t, uint64(0), result.Transfer.In)
	require.Equal(t, uint64(0), result.Transfer.Out)
}

func TestStateStreamDisconnectSideEffectSerializesReplacement(t *testing.T) {
	server := &Server{}
	old := server.AttachStateStream(runtimeOwnershipStream{})
	entered := make(chan struct{})
	release := make(chan struct{})
	cleared := make(chan bool, 1)
	go func() { cleared <- old.ClearWithSideEffect(func() { close(entered); <-release }) }()
	<-entered
	attached := make(chan StateStreamLease, 1)
	go func() { attached <- server.AttachStateStream(runtimeOwnershipStream{}) }()
	select {
	case <-attached:
		close(release)
		t.Fatal("replacement attached before disconnect persistence finished")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	require.True(t, <-cleared)
	next := <-attached
	require.True(t, next.UpdateState(&HostState{}, time.Now()))
	called := false
	require.False(t, old.ClearWithSideEffect(func() { called = true }))
	require.False(t, called)
	require.False(t, old.UpdateStateWithSideEffect(&HostState{}, time.Now(), func() error { called = true; return nil }))
	require.False(t, called)
}

// RuntimeSnapshot serves every server list and the alert loop. It must not wait
// for a state report's side effect (online history, database), nor for a
// replacement stream that does.
func TestStateStreamSideEffectDoesNotBlockRuntimeSnapshot(t *testing.T) {
	busy, idle := &Server{}, &Server{}
	InitServer(busy)
	InitServer(idle)
	lease := busy.AttachStateStream(runtimeOwnershipStream{})
	require.True(t, lease.UpdateState(&HostState{Uptime: 1}, time.Unix(1, 0)))
	previous := busy.RuntimeSnapshot().ReportSequence

	writing, release := make(chan struct{}), make(chan struct{})
	reported := make(chan bool, 1)
	go func() {
		reported <- lease.UpdateStateWithSideEffect(&HostState{Uptime: 2}, time.Unix(2, 0), func() error {
			close(writing)
			<-release
			return nil
		})
	}()
	<-writing
	attached := make(chan StateStreamLease, 1)
	go func() { attached <- busy.AttachStateStream(runtimeOwnershipStream{}) }()
	time.Sleep(20 * time.Millisecond)

	snapshots := make(chan RuntimeSnapshot, 1)
	go func() {
		idle.RuntimeSnapshot()
		snapshots <- busy.RuntimeSnapshot()
	}()
	select {
	case snapshot := <-snapshots:
		// The report is visible as a whole: its state with its own sequence.
		require.Equal(t, uint64(2), snapshot.State.Uptime)
		require.Equal(t, time.Unix(2, 0), snapshot.LastActive)
		require.Greater(t, snapshot.ReportSequence, previous)
	case <-time.After(5 * time.Second):
		t.Error("RuntimeSnapshot waited for a state report's side effect")
	}
	select {
	case <-attached:
		t.Error("replacement attached before the report's side effect finished")
	default:
	}
	close(release)
	require.True(t, <-reported)
	next := <-attached
	require.False(t, lease.UpdateState(&HostState{}, time.Now()))
	require.True(t, next.UpdateState(&HostState{}, time.Now()))
}

func TestServerRuntimeOwnership_revokedLeaseCannotMutateRuntime(t *testing.T) {
	server := &Server{}
	InitServer(server)
	lease := server.AttachStateStream(runtimeOwnershipStream{})
	require.True(t, lease.UpdateState(&HostState{Uptime: 1}, time.Unix(1, 0)))

	server.RevokeStateStream()

	require.False(t, lease.UpdateState(&HostState{Uptime: 2}, time.Unix(2, 0)))
	require.False(t, lease.Clear())
	snapshot := server.RuntimeSnapshot()
	require.Equal(t, uint64(1), snapshot.State.Uptime)
	require.True(t, snapshot.LastActive.IsZero())

	replacement := server.AttachStateStream(runtimeOwnershipStream{})
	require.True(t, replacement.UpdateState(&HostState{Uptime: 3}, time.Unix(3, 0)))
}
