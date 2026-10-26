package singleton

import (
	"context"
	"errors"
	"sync"

	"github.com/nezhahq/nezha/model"
)

// A fixed stripe set bounds lock storage. Ownership publication and sensitive
// operation admission share these locks, closing check-then-transfer races.
var serverControlLocks [256]sync.RWMutex

func lockServerControlTransition(serverID uint64) func() {
	lock := &serverControlLocks[serverID%uint64(len(serverControlLocks))]
	lock.Lock()
	return lock.Unlock
}

func ServerControlTransitioning(serverID uint64) bool {
	transfer := ServerTransferShared
	if transfer == nil {
		return false
	}
	transfer.mu.RLock()
	defer transfer.mu.RUnlock()
	return transfer.initiating[serverID] || transfer.pending[serverID] != nil || transfer.terminalSecretRecovery[serverID] != nil || transfer.revertDeliveries[serverID] != nil
}

// BeginServerControl returns a lease held until task dispatch / stream
// registration finishes. Rollback remains blocked until the agent proves it
// rotated credentials, rather than merely until the transfer row is terminal.
func BeginServerControl(serverID, userID uint64, admin bool) (func(), *model.Server, error) {
	lock := &serverControlLocks[serverID%uint64(len(serverControlLocks))]
	lock.RLock()
	server, ok := ServerShared.Get(serverID)
	if !ok || server == nil || (!admin && (server.GetUserID() != userID || ServerControlTransitioning(serverID))) {
		lock.RUnlock()
		return nil, nil, errors.New("server access denied during ownership or credential transition")
	}
	return lock.RUnlock, server, nil
}

type serverControlIdentityKey struct{}
type serverControlEpochKey struct{}

func WithServerControlEpoch(ctx context.Context, epoch uint64) context.Context {
	return context.WithValue(ctx, serverControlEpochKey{}, epoch)
}

type ServerControlIdentity struct {
	UserID uint64
	Admin  bool
}

func WithServerControlIdentity(ctx context.Context, userID uint64, admin bool) context.Context {
	return context.WithValue(ctx, serverControlIdentityKey{}, ServerControlIdentity{userID, admin})
}
func BeginServerControlFromContext(ctx context.Context, serverID uint64) (func(), error) {
	identity, ok := ctx.Value(serverControlIdentityKey{}).(ServerControlIdentity)
	if !ok { // Internal callers still cannot operate on a transitioning server.
		if ServerControlTransitioning(serverID) {
			return nil, errors.New("server credential transition in progress")
		}
		return func() {}, nil
	}
	release, _, err := BeginServerControl(serverID, identity.UserID, identity.Admin)
	if err != nil {
		return nil, err
	}
	// Recheck the URL generation under the same lease as final stream admission.
	if epoch, bound := ctx.Value(serverControlEpochKey{}).(uint64); bound && epoch != ServerControlEpoch(serverID) {
		release()
		return nil, errors.New("target server ownership changed since URL issuance")
	}
	return release, nil
}

// ServerTaskAdmission resolves both role and ownership at delivery time.
func ServerTaskAdmission(serverID, userID uint64) func() (func(), error) {
	return func() (func(), error) {
		release, _, err := BeginServerControl(serverID, userID, userIsAdmin(userID))
		return release, err
	}
}

// ServerControlEpoch invalidates derived URLs permanently on each transfer.
// Tokens and epochs are both process-local; rollback never restores an old epoch.
func ServerControlEpoch(serverID uint64) uint64 {
	transfer := ServerTransferShared
	if transfer == nil {
		return 0
	}
	transfer.mu.RLock()
	defer transfer.mu.RUnlock()
	return transfer.controlEpoch[serverID]
}
