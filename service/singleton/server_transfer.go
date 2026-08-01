package singleton

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"
	"golang.org/x/mod/semver"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	pb "github.com/nezhahq/nezha/proto"
)

// transferHandshakeSecretLength matches model.DefaultAgentSecretLength so
// agent-side validation that expects char(32) accepts handshake secrets
// without a special case.
const transferHandshakeSecretLength = 32

// MinServerTransferAgentVersion is the minimum agent build version that
// recognises TaskTypeServerTransferApply. Pre-transfer agents see the type
// fall through their `switch task.GetType()` default and never reply, so
// dashboard would wait the full 24h timeout sweep. Refuse the transfer
// up-front instead, with a clear operator-facing reason.
const MinServerTransferAgentVersion = "v1.18.0"

// ServerTransferShared owns the lifecycle of in-flight ServerTransfer rows:
// in-memory pending index used by auth tolerance, state-machine transitions
// (verified / failed / timeout / cancelled), best-effort ApplyConfig push to
// the affected agent, and a fan-out broker for the dashboard WebSocket.
var ServerTransferShared *ServerTransferClass

// ServerTransferStreamRevocationHook is installed by the rpc service at
// startup. It is invoked whenever a transfer transition (Register on
// Initiate, revertTransition on Cancel/Fail/Timeout, OnServersDeleted)
// rotates a server's effective ownership; the rpc package closes every
// IOStream whose targetServerID matches, so a terminal/file-manager/NAT
// session opened by the old owner cannot survive into the new owner's
// tenancy. The dashboard package leaves this nil when running without
// the rpc service (tests).
//
// Singleton can't import rpc directly without a cycle, so we expose the
// hook as a package-level function variable and let cmd/dashboard/rpc
// wire it in ServeRPC.
var ServerTransferStreamRevocationHook func(serverID uint64)

// ServerTransferRevokeStreamsForServer is the dispatch entry the
// state-machine calls. It is safe to call when no hook is installed
// (tests, headless dashboard); revocation simply becomes a no-op.
func ServerTransferRevokeStreamsForServer(serverID uint64) {
	hook := ServerTransferStreamRevocationHook
	if hook == nil {
		return
	}
	hook(serverID)
}

// defaultServerTransferTimeout is the upper bound a Pending transfer may live
// before being auto-failed. Chosen at 24h so an agent that's offline at the
// time of transfer still has a generous window to come back online and pick
// up its new credentials. Cancellable mid-window.
const defaultServerTransferTimeout = 24 * time.Hour

// serverTransferTimeoutTickInterval governs how often the timeout sweeper
// runs. 30s gives near-instant detection on the (rare) timeout cases without
// hammering the DB on a system that's idle most of the time.
const serverTransferTimeoutTickInterval = 30 * time.Second

const defaultRevertDeliveryRecoveryWindow = defaultServerTransferTimeout

// ServerTransferClass is the singleton holding pending transfers and their
// subscribers. All mutating operations go through methods so DB and in-memory
// state stay in sync.
type ServerTransferClass struct {
	mu               sync.RWMutex
	pending          map[uint64]*model.ServerTransfer
	revertDeliveries map[uint64]*model.ServerTransfer
	// revertRecovery holds RevertHandshakeSecrets the dashboard has pushed
	// but the agent has not yet acknowledged, in the window between Cancel/
	// Fail/Timeout and either the agent's reconnect (which MarkRevertDelivered
	// promotes) or expiry. It is consulted by auth via LookupByRevertHandshakeSecret
	// alongside revertDeliveries, but unlike revertDeliveries it is not used
	// to drive new ApplyConfig pushes — that distinction is what lets
	// Register clear revertDeliveries (so a stale pushRevertIfOnline cannot
	// overwrite a freshly-applied new HandshakeSecret on the agent) while
	// still keeping the auth recovery channel open for the agent that may
	// still hold the old RevertHandshakeSecret on disk.
	// terminalSecretRecovery holds the just-terminated transfer for each
	// server so the agent can authenticate during the bounded recovery
	// window even after Cancel/Fail/Timeout. One slot per server covers
	// BOTH per-transfer secrets simultaneously:
	//
	//   forward (t.HandshakeSecret)        — agent committed it to disk via
	//                                        the 10s reload timer before the
	//                                        dashboard observed MarkVerified.
	//                                        Auth admits it but does NOT
	//                                        promote, so RequestTask runs
	//                                        OnAgentReconnect and the
	//                                        rollback ApplyConfig swaps the
	//                                        agent onto the revert secret.
	//
	//   revert  (t.RevertHandshakeSecret)  — dashboard pushed the rollback;
	//                                        the agent has 10s before its
	//                                        reload commits. Auth admits the
	//                                        revert secret and on success
	//                                        promotes it (MarkRevertDelivered)
	//                                        into verifiedHandshakes — that
	//                                        is the agent's stable credential
	//                                        from there on.
	//
	// One slot, two kinds, same TTL (defaultRevertDeliveryRecoveryWindow),
	// same eviction triggers (Register on a NEW transfer for this server
	// for the forward kind only — see below — / MarkRevertDelivered /
	// MarkVerified / OnServersDeleted). Register-on-Retry intentionally
	// preserves the slot so the agent's still-in-flight rollback can
	// recover even while a fresh pending row is being set up.
	//
	// SECURITY: only revertTransition populates this map. A direct DB poke
	// to a terminal status (the attacker-reuse model exercised by
	// TestAuthHandshakeSecretRejectedAfterTransferTerminated) never reaches
	// this code path, so a stolen per-transfer secret cannot authenticate
	// even if the attacker can forge a terminal row in the DB.
	terminalSecretRecovery map[uint64]*model.ServerTransfer
	// verifiedHandshakes maps serverID -> the HandshakeSecret of the most
	// recent Verified transfer that landed on this server. PushIfOnline
	// delivers ONLY the per-transfer HandshakeSecret to the agent, never a
	// long-term user-global AgentSecret, so once MarkVerified completes the
	// agent's persistent on-disk credential for this server IS the handshake
	// secret. Auth has to keep accepting it for that (serverID, secret) pair
	// on every subsequent reconnect, or the agent silently locks itself out
	// the next time the gRPC stream drops. Invalidated when a new transfer
	// is initiated for the same server (Initiate / Register).
	verifiedHandshakes map[uint64]string
	// initiating tracks servers whose InitiateExclusive call is currently
	// running the DB transaction. It exists separately from `pending`
	// because the row hasn't been Registered yet — without this set, two
	// concurrent callers could both pass the HasPending guard, both run
	// their transactions, and both succeed in creating Pending rows.
	initiating map[uint64]bool
	// applyConfigSendLocks orders ApplyConfig sends per server transfer lifecycle.
	// Do not use c.mu for this: stream.Send may block, but stale new-secret
	// pushes and cancel/fail/timeout revert pushes must not overtake each other
	// for the same server because the agent applies the last task it receives.
	applyConfigSendLocks sync.Map

	subMu     sync.Mutex
	subs      map[uint64]chan *model.ServerTransfer
	nextSubID uint64

	timeout  time.Duration
	stopOnce sync.Once
	stopCh   chan struct{}
}

// ErrServerAlreadyTransferring is returned by InitiateExclusive when a
// concurrent caller has already claimed the server for a new transfer (or a
// Pending row already exists). Callers that surface a structured outcome
// (batch-move, retry) should detect it with errors.Is and translate to
// their domain-specific status.
var ErrServerAlreadyTransferring = errors.New("server already has an in-flight transfer")

// ErrAgentTooOldForTransfer is returned by InitiateExclusive when the agent's
// reported build version is older than MinServerTransferAgentVersion and
// therefore does not understand TaskTypeServerTransferApply. Refusing the
// transfer up-front avoids a 24h timeout sweep on an agent that will never
// reply. If the agent has never connected (Server.Host == nil) the check is
// deferred to OnAgentReconnect / PushIfOnline.
var ErrAgentTooOldForTransfer = fmt.Errorf("agent build older than %s does not support server transfer (TaskTypeServerTransferApply)", MinServerTransferAgentVersion)

// agentSupportsTransfer reports whether s has reported a build version >=
// MinServerTransferAgentVersion. Returns true when version is unknown (agent
// never reported) so callers can defer the decision; PushIfOnline re-checks
// at push time.
func agentSupportsTransfer(s *model.Server) bool {
	if s == nil {
		return true
	}
	runtime := s.RuntimeSnapshot()
	if runtime.Host == nil {
		return true
	}
	v := strings.TrimSpace(runtime.Host.Version)
	if v == "" {
		return true
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return true
	}
	return semver.Compare(v, MinServerTransferAgentVersion) >= 0
}

// NewServerTransferClass loads any persisted Pending transfers from the DB
// into the in-memory index and starts the timeout sweeper. Called from
// LoadSingleton.
func NewServerTransferClass() *ServerTransferClass {
	c := &ServerTransferClass{
		pending:                make(map[uint64]*model.ServerTransfer),
		revertDeliveries:       make(map[uint64]*model.ServerTransfer),
		terminalSecretRecovery: make(map[uint64]*model.ServerTransfer),
		verifiedHandshakes:     make(map[uint64]string),
		initiating:             make(map[uint64]bool),
		subs:                   make(map[uint64]chan *model.ServerTransfer),
		timeout:                defaultServerTransferTimeout,
		stopCh:                 make(chan struct{}),
	}

	var pending []model.ServerTransfer
	// 不要再吞掉这个错误：旧代码直接 DB.Where(...).Find(&pending) 忽略
	// res.Error，schema 损坏 / 表丢失 / DB 锁等情况下 pending 会被静默
	// 留空，所有进行中的 transfer 在 dashboard 重启后就丢失了 auth 容忍窗口，
	// 对应 agent 会在重连时被拒绝。GORM 默认 logger 也会打这条 SQL，但混在
	// SQL 日志里很难被注意到；这里显式发一条 NEZHA>> 前缀让运维能立刻看到。
	if res := DB.Where("status = ?", model.ServerTransferStatusPending).Find(&pending); res.Error != nil {
		log.Printf("NEZHA>> ServerTransferClass: failed to load pending transfers from DB: %v", res.Error)
	}
	for i := range pending {
		t := pending[i]
		// Ghost guard: the server may have been hard-deleted while the
		// dashboard was down. Skipping orphans keeps HasPending honest
		// (no false positives blocking new transfers) and prevents the
		// timeout sweeper from looping forever on a row whose server
		// row no longer exists.
		if server, ok := ServerShared.Get(t.ServerID); !ok || server == nil {
			log.Printf("NEZHA>> ServerTransferClass: dropping pending transfer %d for missing server %d", t.ID, t.ServerID)
			continue
		}
		c.pending[t.ServerID] = &t
	}

	var reverted []model.ServerTransfer
	// acked_at IS NULL is non-negotiable: MarkRevertDelivered persists
	// acked_at the moment the agent has provably rotated to the rollback
	// credential and intentionally clears the in-memory delivery + recovery
	// slots to close the auth tolerance window. Without filtering on
	// acked_at here, every dashboard restart within
	// defaultRevertDeliveryRecoveryWindow rehydrates the consumed rollback
	// into revertDeliveries / terminalSecretRecovery and reopens the
	// LookupRevertDelivery + LookupByTerminalSecretRecovery paths in
	// service/rpc/auth.go — readmitting the rolled-back ToUserID's global
	// AgentSecret long after the rollback has been delivered. ACKed rows
	// are rebuilt below into verifiedHandshakes from the same acked_at,
	// so the long-term credential the agent actually holds on disk still
	// authenticates.
	if res := DB.
		Where("status IN ? AND updated_at >= ? AND acked_at IS NULL", []model.ServerTransferStatus{
			model.ServerTransferStatusFailed,
			model.ServerTransferStatusTimeout,
			model.ServerTransferStatusCancelled,
		}, time.Now().Add(-defaultRevertDeliveryRecoveryWindow)).
		Order("updated_at ASC").
		Find(&reverted); res.Error != nil {
		log.Printf("NEZHA>> ServerTransferClass: failed to load reverted transfer deliveries from DB: %v", res.Error)
	}
	for i := range reverted {
		t := reverted[i]
		server, ok := ServerShared.Get(t.ServerID)
		if !ok || server == nil || server.GetUserID() != t.FromUserID {
			continue
		}
		c.revertDeliveries[t.ServerID] = &t
		c.terminalSecretRecovery[t.ServerID] = &t
	}

	// Rebuild verifiedHandshakes by merging Verified rows and acked rollback
	// rows and picking, per server, the credential whose AckedAt is the
	// newest. That AckedAt is the moment the agent provably rotated to that
	// secret — so the newest one is the one currently on disk. The old
	// two-pass "Verified first, rollback only fills empty slots" approach
	// stranded the agent in the chained transfer+rollback case where the
	// rollback credential is newer than the older Verified credential.
	var verified []model.ServerTransfer
	if res := DB.
		Where("status = ? AND acked_at IS NOT NULL", model.ServerTransferStatusVerified).
		Find(&verified); res.Error != nil {
		log.Printf("NEZHA>> ServerTransferClass: failed to load verified transfers from DB: %v", res.Error)
	}
	var rollbackAcked []model.ServerTransfer
	if res := DB.
		Where("status IN ? AND acked_at IS NOT NULL", []model.ServerTransferStatus{
			model.ServerTransferStatusFailed,
			model.ServerTransferStatusTimeout,
			model.ServerTransferStatusCancelled,
		}).
		Find(&rollbackAcked); res.Error != nil {
		log.Printf("NEZHA>> ServerTransferClass: failed to load acked rollback transfers from DB: %v", res.Error)
	}

	type credCandidate struct {
		serverID   uint64
		transferID uint64
		secret     string
		ackedAt    time.Time
		isRevert   bool
		toUserID   uint64
	}
	candidates := make([]credCandidate, 0, len(verified)+len(rollbackAcked))
	for i := range verified {
		t := verified[i]
		if t.HandshakeSecret == "" || t.AckedAt == nil {
			continue
		}
		candidates = append(candidates, credCandidate{
			serverID:   t.ServerID,
			transferID: t.ID,
			secret:     t.HandshakeSecret,
			ackedAt:    *t.AckedAt,
			toUserID:   t.ToUserID,
		})
	}
	for i := range rollbackAcked {
		t := rollbackAcked[i]
		if t.RevertHandshakeSecret == "" || t.AckedAt == nil {
			continue
		}
		candidates = append(candidates, credCandidate{
			serverID:   t.ServerID,
			transferID: t.ID,
			secret:     t.RevertHandshakeSecret,
			ackedAt:    *t.AckedAt,
			isRevert:   true,
			toUserID:   t.FromUserID,
		})
	}
	// Sort newest-first by AckedAt, breaking ties with transferID. AckedAt
	// alone is not enough on platforms whose time.Now() granularity is
	// coarse (Windows: ~15.6ms): MarkVerified and the immediately-following
	// MarkRevertDelivered routinely produce identical timestamps, and a
	// stable sort then leaves the Verified candidate (appended first) ahead
	// of the rollback that is actually on disk, locking the agent out on
	// restart. transferID is monotonically increasing within a server's
	// transfer lifecycle, so the later rotation always wins the tiebreak.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].ackedAt.Equal(candidates[j].ackedAt) {
			return candidates[i].transferID > candidates[j].transferID
		}
		return candidates[i].ackedAt.After(candidates[j].ackedAt)
	})

	for _, cand := range candidates {
		if _, alreadySeen := c.verifiedHandshakes[cand.serverID]; alreadySeen {
			continue
		}
		server, ok := ServerShared.Get(cand.serverID)
		if !ok || server == nil {
			continue
		}
		// Forward Verified credential is accepted when either the server
		// still belongs to ToUserID (steady state) or a subsequent transfer
		// is Pending whose FromUserID equals this ToUserID (chained-transfer
		// rollover window — agent on disk still holds the previous
		// HandshakeSecret until MarkVerified on the new transfer).
		// Rollback credential is accepted only when current owner still
		// equals the original FromUserID (the rollback target).
		if cand.isRevert {
			if server.GetUserID() != cand.toUserID {
				continue
			}
		} else {
			if server.GetUserID() != cand.toUserID {
				if pending, hasPending := c.pending[cand.serverID]; !hasPending || pending.FromUserID != cand.toUserID {
					continue
				}
			}
		}
		c.verifiedHandshakes[cand.serverID] = cand.secret
	}

	go c.timeoutSweepLoop()
	return c
}

// Stop terminates the background timeout sweeper. Intended for tests; in
// production the singleton lives for the lifetime of the process.
func (c *ServerTransferClass) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
}

// LookupPending returns the pending transfer for a server if one exists.
// Hot path: called from authorizeAgentForUUID on every agent RPC, so it
// uses an RWMutex and a map lookup only.
func (c *ServerTransferClass) LookupPending(serverID uint64) (*model.ServerTransfer, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.pending[serverID]
	return t, ok
}

// HasPending reports whether the given server has an in-flight transfer.
// Used by Initiate to enforce the "one active transfer per server" invariant.
func (c *ServerTransferClass) HasPending(serverID uint64) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.pending[serverID]
	return ok
}

func (c *ServerTransferClass) LookupRevertDelivery(serverID uint64) (*model.ServerTransfer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.revertDeliveries[serverID]
	if ok && t.UpdatedAt.Before(time.Now().Add(-defaultRevertDeliveryRecoveryWindow)) {
		delete(c.revertDeliveries, serverID)
		return nil, false
	}
	return t, ok
}

func (c *ServerTransferClass) ClearRevertDelivery(serverID, transferID uint64) {
	c.mu.Lock()
	if t, ok := c.revertDeliveries[serverID]; ok && t.ID == transferID {
		delete(c.revertDeliveries, serverID)
	}
	c.mu.Unlock()
}

// MarkRevertDelivered is called from the auth path the first time the agent
// authenticates with a transfer's RevertHandshakeSecret. The agent has now
// persisted that secret as its on-disk credential (handleApplyConfigTask's
// 10s timer has fired and applyPendingReload has saved + published it), so
// it is the long-term credential for this server until another transfer
// rotates it again. Promote it into verifiedHandshakes — the auth-path
// long-term map — and persist AckedAt so dashboard restart can rebuild.
// Without this, the only acceptance path is LookupByRevertHandshakeSecret,
// which prunes after defaultRevertDeliveryRecoveryWindow and leaves the
// agent permanently locked out.
func (c *ServerTransferClass) MarkRevertDelivered(serverID, transferID uint64) error {
	now := time.Now()
	res := DB.Model(&model.ServerTransfer{}).
		Where("id = ? AND status IN ? AND acked_at IS NULL", transferID, []model.ServerTransferStatus{
			model.ServerTransferStatusFailed,
			model.ServerTransferStatusTimeout,
			model.ServerTransferStatusCancelled,
		}).
		Update("acked_at", &now)
	if res.Error != nil {
		return res.Error
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Agent has rotated to the revert secret on disk, so the entire
	// per-server terminal-recovery slot (covering both forward and revert
	// kinds for THIS transfer) is now stale. Promote the revert secret
	// into verifiedHandshakes first so the long-term credential is in
	// place before we drop the bounded recovery entry.
	if t, ok := c.terminalSecretRecovery[serverID]; ok && t.ID == transferID && t.RevertHandshakeSecret != "" {
		c.verifiedHandshakes[serverID] = t.RevertHandshakeSecret
		t.AckedAt = &now
		delete(c.terminalSecretRecovery, serverID)
	}
	// revertDeliveries is the push queue (drives pushRevertIfOnline); it
	// can lag terminalSecretRecovery when Register-on-Retry already
	// dropped the push entry. Clear by id only — a newer transfer's push
	// entry must survive.
	if t, ok := c.revertDeliveries[serverID]; ok && t.ID == transferID {
		delete(c.revertDeliveries, serverID)
	}
	return nil
}

// LookupByHandshakeSecret returns the Pending transfer whose per-transfer
// HandshakeSecret matches secret, or (nil, false). Called from the gRPC auth
// path so an agent that received the ApplyConfig and reconnected under the
// handshake secret can be authenticated without exposing the destination
// user's global AgentSecret. O(n) over the pending map: n is bounded by the
// count of in-flight transfers, in practice tiny.
func (c *ServerTransferClass) LookupByHandshakeSecret(secret string) (*model.ServerTransfer, bool) {
	if secret == "" {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, t := range c.pending {
		if t.HandshakeSecret == secret {
			return t, true
		}
	}
	return nil, false
}

// LookupServerByVerifiedHandshakeSecret returns the server ID whose most
// recent Verified transfer's HandshakeSecret equals secret. Called from the
// auth path on every reconnect that misses the pending-handshake and
// revert-handshake lookups, so a Verified agent — whose persisted
// credential is the per-transfer handshake secret because no final-rotation
// ApplyConfig ever swaps it out — keeps authenticating across stream drops
// and restarts. O(n) over the verifiedHandshakes map, n is bounded by the
// number of distinct servers that have ever completed a transfer in this
// process's lifetime; in practice tiny relative to total auth traffic, and
// only consulted when the global secret lookup is about to fail.
func (c *ServerTransferClass) LookupServerByVerifiedHandshakeSecret(secret string) (uint64, bool) {
	if secret == "" {
		return 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for serverID, s := range c.verifiedHandshakes {
		if s == secret {
			return serverID, true
		}
	}
	return 0, false
}

// TerminalRecoveryKind distinguishes which per-transfer secret matched
// inside terminalSecretRecovery so auth can pick the right post-match
// behaviour: forward → admit but do NOT promote (rollback delivery still
// has to happen); revert → admit and trigger MarkRevertDelivered to
// promote into verifiedHandshakes.
type TerminalRecoveryKind uint8

const (
	TerminalRecoveryNone TerminalRecoveryKind = iota
	TerminalRecoveryForward
	TerminalRecoveryRevert
)

// LookupByTerminalSecretRecovery is the single auth-facing entry into
// terminalSecretRecovery. Both per-kind wrappers delegate here so there is
// exactly one TTL-prune + secret-match site to audit. Returns the matched
// transfer and which secret matched.
func (c *ServerTransferClass) LookupByTerminalSecretRecovery(secret string) (*model.ServerTransfer, TerminalRecoveryKind, bool) {
	if secret == "" {
		return nil, TerminalRecoveryNone, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cutoff := time.Now().Add(-defaultRevertDeliveryRecoveryWindow)
	for serverID, t := range c.terminalSecretRecovery {
		if t.UpdatedAt.Before(cutoff) {
			delete(c.terminalSecretRecovery, serverID)
			continue
		}
		if t.HandshakeSecret == secret {
			return t, TerminalRecoveryForward, true
		}
		if t.RevertHandshakeSecret == secret {
			return t, TerminalRecoveryRevert, true
		}
	}
	return nil, TerminalRecoveryNone, false
}

// LookupByRevertHandshakeSecret keeps the prior per-kind signature so
// callers outside the singleton (auth.go's promote-on-success path) do
// not need to know about the unified table. Only returns matches with
// kind=revert.
func (c *ServerTransferClass) LookupByRevertHandshakeSecret(secret string) (*model.ServerTransfer, bool) {
	t, kind, ok := c.LookupByTerminalSecretRecovery(secret)
	if !ok || kind != TerminalRecoveryRevert {
		return nil, false
	}
	return t, true
}

// LookupByForwardHandshakeSecretInTerminalRecovery is the symmetric
// per-kind wrapper for the forward secret. Only returns matches with
// kind=forward.
func (c *ServerTransferClass) LookupByForwardHandshakeSecretInTerminalRecovery(secret string) (*model.ServerTransfer, bool) {
	t, kind, ok := c.LookupByTerminalSecretRecovery(secret)
	if !ok || kind != TerminalRecoveryForward {
		return nil, false
	}
	return t, true
}

func (c *ServerTransferClass) registerRevertDelivery(t *model.ServerTransfer) {
	c.mu.Lock()
	c.revertDeliveries[t.ServerID] = t
	c.mu.Unlock()
}

// registerTerminalSecretRecovery records the just-terminated transfer so
// auth can recognise either of its per-transfer secrets during the bounded
// recovery window. One call per revertTransition; the per-server slot is
// overwritten by a later terminal transition, mirroring the behaviour
// agents experience on disk (last credential applied wins).
func (c *ServerTransferClass) registerTerminalSecretRecovery(t *model.ServerTransfer) {
	if t.HandshakeSecret == "" && t.RevertHandshakeSecret == "" {
		return
	}
	c.mu.Lock()
	c.terminalSecretRecovery[t.ServerID] = t
	c.mu.Unlock()
}

func (c *ServerTransferClass) applyConfigSendLock(serverID uint64) *sync.Mutex {
	lock, _ := c.applyConfigSendLocks.LoadOrStore(serverID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// Initiate runs inside the given transaction and:
//   - creates the ServerTransfer row with Status=Pending
//   - flips Server.UserID to toUserID
//
// Caller is responsible for ensuring no concurrent transfer exists for
// serverID (HasPending check earlier in the same critical section) and for
// invoking Register + PushIfOnline after the transaction commits.
func (c *ServerTransferClass) Initiate(tx *gorm.DB, serverID, fromUserID, toUserID, initiatorID uint64) (*model.ServerTransfer, error) {
	// Generate both handshake secrets up-front. PushIfOnline embeds
	// HandshakeSecret in the agent ApplyConfig instead of the destination
	// user's global AgentSecret; the rollback path mirrors with
	// RevertHandshakeSecret. Per-transfer scope: a leak to a hijacked stream
	// gives the attacker only this one server's rotation token, never the
	// user's global secret. Generation must succeed — falling back to the
	// global secret here would silently reintroduce the cross-user leak.
	handshake, err := utils.GenerateRandomString(transferHandshakeSecretLength)
	if err != nil {
		return nil, fmt.Errorf("generate transfer handshake secret: %w", err)
	}
	revertHandshake, err := utils.GenerateRandomString(transferHandshakeSecretLength)
	if err != nil {
		return nil, fmt.Errorf("generate transfer revert handshake secret: %w", err)
	}
	t := &model.ServerTransfer{
		ServerID:              serverID,
		FromUserID:            fromUserID,
		ToUserID:              toUserID,
		InitiatorID:           initiatorID,
		Status:                model.ServerTransferStatusPending,
		HandshakeSecret:       handshake,
		RevertHandshakeSecret: revertHandshake,
	}
	if err := tx.Create(t).Error; err != nil {
		return nil, err
	}
	// RowsAffected==1 is the only signal that a real server row was mutated:
	// if the row was deleted between the caller's pre-check and this UPDATE,
	// returning success would let Register publish a ghost pending entry and
	// auth.go would then keep accepting the previous owner's secret for a
	// server that doesn't exist. Surface the divergence so the surrounding
	// transaction rolls back the orphan ServerTransfer.
	res := tx.Model(&model.Server{}).Where("id = ?", serverID).Update("user_id", toUserID)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, fmt.Errorf("server %d: ownership update affected %d rows (want 1) — row likely deleted concurrently", serverID, res.RowsAffected)
	}
	return t, nil
}

// Register makes a freshly-persisted Pending transfer visible to the auth
// tolerance path. Must be called only after the Initiate transaction has
// committed, otherwise authorizeAgentForUUID could observe a transfer that
// doesn't yet exist in the DB.
//
// Ordering invariant: the in-memory Server.UserID is updated BEFORE the
// pending entry is published. Inverting these two would leave a window
// where authorizeAgentForUUID still sees the old owner via ServerShared
// and admits the old AgentSecret on the happy "owner match" path —
// bypassing the bounded pending-tolerance contract.
func (c *ServerTransferClass) Register(t *model.ServerTransfer) {
	// SetUserID uses an atomic write because auth.go reads this hot-path field
	// concurrently; ServerClass also serializes it with service reports.
	ServerShared.setUserID(t.ServerID, t.ToUserID)

	c.mu.Lock()
	c.pending[t.ServerID] = t
	// Drop only the push queue entry: pushRevertIfOnline must not re-send
	// the prior rollback now that a new transfer is taking over the agent's
	// credential. The auth-side recovery for the prior transfer's secrets
	// stays alive in terminalSecretRecovery — the agent's 10s reload may
	// not have committed the rollback yet and we still need to admit either
	// the previous forward HandshakeSecret (last-completed Verified) or
	// the previous RevertHandshakeSecret (uncommitted rollback) until
	// MarkVerified on this fresh transfer supersedes both.
	delete(c.revertDeliveries, t.ServerID)
	// Do NOT delete verifiedHandshakes[t.ServerID] here. The agent's on-disk
	// credential is the previous HandshakeSecret (PushIfOnline never
	// delivers a user-global secret), and that secret must keep
	// authenticating for the entire rollover: Register precedes PushIfOnline,
	// the agent's reload timer adds another ~10s delay, and PushIfOnline is
	// best-effort against stream loss. MarkVerified replaces the entry with
	// the new HandshakeSecret once the agent has provably rotated;
	// Cancel/Fail/Timeout leave it in place so the agent stays online while
	// ownership rolls back.
	c.mu.Unlock()

	// Ownership has rotated to ToUserID — tear down any IOStream the
	// previous owner had open against this server so it cannot survive
	// into the new tenancy.
	ServerTransferRevokeStreamsForServer(t.ServerID)

	c.broadcast(t)
}

// InitiateExclusive runs the full create-and-publish flow for a new
// ServerTransfer with mutual exclusion on serverID. The HasPending check,
// the DB transaction, and the Register call are serialized via a per-server
// claim so two concurrent callers (e.g. two operators submitting batch-move
// at the same instant) cannot both pass the guard and end up creating two
// Pending rows for the same server. Without this, the older HasPending +
// Initiate + Register sequence had a TOCTOU window — both callers would
// observe "no pending", both run their tx, both Register, with the second
// Register silently overwriting the first in the in-memory index while two
// rows remained Pending in the DB.
//
// Returns ErrServerAlreadyTransferring when a Pending row already exists or
// another caller currently holds the claim. The caller is responsible for
// PushIfOnline after a successful return.
func (c *ServerTransferClass) InitiateExclusive(serverID, fromUserID, toUserID, initiatorID uint64) (*model.ServerTransfer, error) {
	if s, ok := ServerShared.Get(serverID); ok && !agentSupportsTransfer(s) {
		return nil, ErrAgentTooOldForTransfer
	}
	c.mu.Lock()
	if _, hasPending := c.pending[serverID]; hasPending {
		c.mu.Unlock()
		return nil, ErrServerAlreadyTransferring
	}
	if c.initiating[serverID] {
		c.mu.Unlock()
		return nil, ErrServerAlreadyTransferring
	}
	c.initiating[serverID] = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.initiating, serverID)
		c.mu.Unlock()
	}()

	var created *model.ServerTransfer
	err := DB.Transaction(func(tx *gorm.DB) error {
		t, err := c.Initiate(tx, serverID, fromUserID, toUserID, initiatorID)
		if err != nil {
			return err
		}
		created = t
		return nil
	})
	if err != nil {
		return nil, err
	}

	c.Register(created)
	return created, nil
}

// PushIfOnline best-effort sends an ApplyConfig task carrying the transfer's
// per-transfer HandshakeSecret to the affected agent. The destination user's
// global AgentSecret is intentionally NOT embedded: during Pending the agent
// stream is still authenticated by the OLD owner's secret (auth tolerance),
// and a malicious previous owner who hijacks the stream would otherwise
// recover a secret that grants access to every agent that destination user
// owns. HandshakeSecret is scoped to this single transfer and UUID; even if
// it leaks, the blast radius is one server. If the agent is offline
// (TaskStream nil), the push is skipped — OnAgentReconnect will retry when
// the agent returns. Errors are not surfaced; agent failure to apply is
// detected via the explicit TaskResult or the timeout sweeper.
//
// Stale-transfer guard: callers such as OnAgentReconnect look up the pending
// transfer and then call PushIfOnline, but a concurrent Cancel/MarkFailed/
// MarkTimeout can settle the row between those two steps. The agent treats
// later ApplyConfig tasks as supersedes (last arrival wins inside the 10s
// reload window), so a stale push that races past pushRevertIfOnline would
// commit the rejected secret and lock the agent out. Re-check pending state
// right before Send to keep the push consistent with the dashboard's
// authoritative view.
func (c *ServerTransferClass) PushIfOnline(t *model.ServerTransfer) {
	s, ok := ServerShared.Get(t.ServerID)
	if !ok || s == nil {
		return
	}
	stream := s.GetTaskStream()
	if stream == nil {
		return
	}

	if !agentSupportsTransfer(s) {
		if _, err := c.MarkFailed(t.ID, ErrAgentTooOldForTransfer.Error()); err != nil {
			log.Printf("NEZHA>> ServerTransfer PushIfOnline: MarkFailed for too-old agent %d failed: %v", t.ServerID, err)
		}
		return
	}

	if current, ok := c.LookupPending(t.ServerID); !ok || current.ID != t.ID {
		return
	}

	if t.HandshakeSecret == "" {
		// Defence against a legacy Pending row loaded from a pre-fix DB
		// snapshot. Without a handshake secret we have nothing safe to send;
		// the operator must cancel and re-initiate the transfer.
		log.Printf("NEZHA>> ServerTransfer PushIfOnline: transfer %d has empty HandshakeSecret; refusing to fall back to user-global AgentSecret", t.ID)
		return
	}

	payload, err := json.Marshal(map[string]string{
		"client_secret": t.HandshakeSecret,
	})
	if err != nil {
		return
	}

	task := &pb.Task{
		Id:   t.ID,
		Type: model.TaskTypeServerTransferApply,
		Data: string(payload),
	}
	lock := c.applyConfigSendLock(t.ServerID)
	lock.Lock()
	defer lock.Unlock()
	if current, ok := c.LookupPending(t.ServerID); !ok || current.ID != t.ID {
		return
	}
	c.sendApplyConfigTask(s, stream, task)
}

// OnAgentReconnect is invoked by the gRPC RequestTask handler right after
// the new TaskStream is attached. If a Pending transfer exists for this
// server, push the ApplyConfig task — the agent reconnected with the old
// secret (the only secret it knows so far), so this is the moment to deliver
// the new one.
func (c *ServerTransferClass) OnAgentReconnect(serverID uint64) {
	t, ok := c.LookupPending(serverID)
	if ok {
		c.PushIfOnline(t)
		return
	}
	if t, ok := c.LookupRevertDelivery(serverID); ok {
		c.pushRevertIfOnline(t)
	}
}

// pushRevertIfOnline best-effort sends an ApplyConfig task carrying the
// transfer's per-transfer RevertHandshakeSecret, instructing the agent to
// either skip or overwrite the swap it was about to perform. The source
// user's global AgentSecret is intentionally NOT embedded: after a Verified
// rollover the stream is authenticated by the NEW owner, and revealing the
// previous owner's user-global secret would compromise every agent that
// user owns. Used by revertTransition (Cancel / MarkFailed / MarkTimeout)
// to keep the agent's view of the credential in sync with the dashboard's
// reverted Server.UserID.
//
// Without this counter-push, an operator who cancels within the agent's 10s
// reload window leaves a permanent split-brain: the agent commits the swap to
// the rejected new secret and immediately fails auth because the dashboard
// has already restored ownership to FromUserID. The agent's ApplyConfig
// supersede behaviour relies on this counter-push to actually be delivered
// during the 10s window — that's the entire reason supersede exists.
//
// Best-effort: agent offline is fine if it never received the original task.
// If it already switched secrets before the revert landed, the reverted
// transfer is kept as a reconnect-delivery until the old secret is restored.
func (c *ServerTransferClass) pushRevertIfOnline(t *model.ServerTransfer) {
	s, ok := ServerShared.Get(t.ServerID)
	if !ok || s == nil {
		return
	}
	stream := s.GetTaskStream()
	if stream == nil {
		return
	}

	if t.RevertHandshakeSecret == "" {
		log.Printf("NEZHA>> ServerTransfer pushRevertIfOnline: transfer %d has empty RevertHandshakeSecret; refusing to fall back to user-global AgentSecret", t.ID)
		return
	}

	payload, err := json.Marshal(map[string]string{
		"client_secret": t.RevertHandshakeSecret,
	})
	if err != nil {
		return
	}

	task := &pb.Task{
		Id:   t.ID,
		Type: model.TaskTypeServerTransferApply,
		Data: string(payload),
	}
	lock := c.applyConfigSendLock(t.ServerID)
	lock.Lock()
	defer lock.Unlock()
	// Re-check revertDelivery currency inside the send lock. Without this, a
	// concurrent Retry can install a new pending transfer (clearing
	// revertDeliveries[serverID]) and have PushIfOnline win the lock first to
	// deliver the new-owner secret; pushRevertIfOnline then acquires the lock
	// next and Sends the old-owner rollback, which the agent's last-arrival
	// supersede commits — silently rolling back the just-applied new secret
	// and leaving the fresh transfer Pending until the 24h timeout sweep.
	// Mirrors the in-lock LookupPending guard PushIfOnline uses at line ~338.
	if current, ok := c.LookupRevertDelivery(t.ServerID); !ok || current.ID != t.ID {
		return
	}
	// Send-success does NOT mean the agent has rotated yet: handleApplyConfigTask
	// schedules the credential swap on a 10s time.AfterFunc, so the agent only
	// reconnects under RevertHandshakeSecret well after Send returns. Clearing
	// the recovery record here would close LookupByRevertHandshakeSecret before
	// that reconnect arrives, falling through to the global-secret table that
	// doesn't know the per-transfer token — and the agent ends up permanently
	// locked out. Leave the record in place; it will be cleared on one of:
	//   (a) auth.go observing a successful reconnect under RevertHandshakeSecret
	//       (the agent has provably finished applying the rollback),
	//   (b) a Retry/Register installing a newer transfer for this server,
	//   (c) the natural defaultRevertDeliveryRecoveryWindow expiry sweep.
	_ = c.sendApplyConfigTask(s, stream, task)
}

func (c *ServerTransferClass) sendApplyConfigTask(s *model.Server, stream pb.NezhaService_RequestTaskServer, task *pb.Task) error {
	// Keep Send synchronous under the per-server lock. A goroutine+timeout cannot
	// cancel grpc.ServerStream.Send; returning early would let a stale new-secret
	// ApplyConfig complete after a cancel/fail revert and overwrite the rollback.
	//
	// Route through Server.SendTask so the holder-scoped send mutex is
	// honoured: cron / MCP CallAgent / MCP fs.transfer dispatch on the same
	// gRPC stream and would otherwise race grpc-go's one-SendMsg-per-stream
	// invariant. The captured stream argument is still passed to
	// ClearTaskStreamIfCurrent so a reconnect mid-Send cannot wipe a newer
	// published stream when Send fails on the stale one.
	if err := s.SendTask(task); err != nil {
		log.Printf("NEZHA>> ServerTransfer ApplyConfig send failed: serverID=%d transferID=%d: %v", s.ID, task.Id, err)
		s.ClearTaskStreamIfCurrent(stream)
		return err
	}
	return nil
}

// MarkVerified finalizes a pending transfer after the agent has successfully
// reconnected under the new owner's secret.
//
// Return tuple:
//   - (t, nil)    — this call transitioned the row to Verified
//   - (nil, nil)  — idempotent no-op (no pending entry, or a concurrent caller
//     already settled the row out of Pending so RowsAffected=0)
//   - (nil, err)  — DB-level failure during the CAS UPDATE; caller MUST log
//     it or the auth-tolerance window stays open silently for this server
//
// The old signature returned (*ServerTransfer, bool) which conflated the
// idempotent no-op and the DB-error cases, so a broken DB looked identical to
// "already verified" and operators got no signal. The auth path now logs the
// error path explicitly; do not collapse the three return shapes back into a
// bool.
//
// The status update is gated by a WHERE clause so concurrent Cancel or
// timeout sweep cannot race past it: if status is no longer Pending in the
// DB, the UPDATE affects zero rows and the in-memory state is left alone.
// MarkVerified atomically transitions a Pending transfer to Verified.
//
// Invariant: c.mu is held across the DB CAS, the in-memory pending delete,
// and the verifiedHandshakes write. Callers reading c.pending under c.mu
// (auth.go's tolerance window) therefore can never observe a state where
// the DB row is Verified but c.pending still flags the transfer as Pending —
// the auth-bypass window that would otherwise let either a stale
// HandshakeSecret or the old owner's global AgentSecret authenticate
// between the two updates.
//
// Returns verified=true exactly when this call performed the Pending →
// Verified transition for the supplied (serverID, transferID). All other
// outcomes (no pending, transfer id mismatch, lost CAS, DB error) return
// verified=false and the caller (auth) must reject the credential.
func (c *ServerTransferClass) MarkVerified(serverID, transferID uint64) (verified bool, transfer *model.ServerTransfer, err error) {
	c.mu.Lock()

	t, ok := c.pending[serverID]
	if !ok || t.ID != transferID {
		c.mu.Unlock()
		return false, nil, nil
	}

	now := time.Now()
	res := DB.Model(&model.ServerTransfer{}).
		Where("id = ? AND status = ?", t.ID, model.ServerTransferStatusPending).
		Updates(map[string]any{
			"status":   model.ServerTransferStatusVerified,
			"acked_at": &now,
		})
	if res.Error != nil {
		c.mu.Unlock()
		return false, nil, res.Error
	}
	if res.RowsAffected == 0 {
		// Concurrent caller settled the row to a terminal status. The
		// in-memory pending entry is now stale; drop it so the next auth
		// call cannot read it. Do NOT promote any handshake secret.
		delete(c.pending, t.ServerID)
		c.mu.Unlock()
		return false, nil, nil
	}
	t.Status = model.ServerTransferStatusVerified
	t.AckedAt = &now

	delete(c.pending, t.ServerID)
	// Promote the handshake secret to this server's long-term credential:
	// PushIfOnline delivered ONLY HandshakeSecret to the agent, so the
	// agent's persisted on-disk client_secret is exactly this string and
	// every future reconnect presents it. Auth's verified-handshake lookup
	// uses this map to keep accepting the credential after the pending
	// entry has been removed.
	if t.HandshakeSecret != "" {
		c.verifiedHandshakes[t.ServerID] = t.HandshakeSecret
	}
	delete(c.terminalSecretRecovery, t.ServerID)
	c.mu.Unlock()

	c.broadcast(t)
	return true, t, nil
}

// MarkFailed transitions a pending transfer to Failed with the supplied
// reason and reverts Server.UserID back to FromUserID. Used by the RPC
// handler when an agent reports an explicit failure via TaskResult.
func (c *ServerTransferClass) MarkFailed(transferID uint64, reason string) (*model.ServerTransfer, error) {
	return c.revertTransition(transferID, model.ServerTransferStatusFailed, reason)
}

// MarkTimeout transitions a pending transfer to Timeout and reverts
// Server.UserID. Invoked by the timeout sweeper.
func (c *ServerTransferClass) MarkTimeout(transferID uint64) (*model.ServerTransfer, error) {
	return c.revertTransition(transferID, model.ServerTransferStatusTimeout, "")
}

// Cancel transitions a pending transfer to Cancelled and reverts
// Server.UserID. Permission filtering happens at the HTTP layer; this method
// trusts the caller and only enforces "still Pending" via CAS.
func (c *ServerTransferClass) Cancel(transferID uint64) (*model.ServerTransfer, error) {
	return c.revertTransition(transferID, model.ServerTransferStatusCancelled, "")
}

// Retry creates a new Pending transfer with the same From/To as an existing
// terminal transfer. Used by the dashboard to re-issue after a timeout or
// failure without forcing the operator to retype the target user. Concurrent
// safety against another in-flight transfer is delegated to
// InitiateExclusive (same TOCTOU-free contract batch-move relies on).
//
// 必须校验 s.UserID == prev.FromUserID：操作员在 dashboard 上看到的是
// "prev.FromUserID → prev.ToUserID" 这条记录，如果在 retry 之前有别的并发
// transfer 把 server 划到了第三个用户，旧逻辑会用「当前 owner」当
// FromUserID，悄悄发出一条语义完全不同的 transfer（"new_owner → prev.To"）。
// 强制要求当前 owner 仍是 prev.FromUserID，否则报错让操作员重新发起。
// Retry creates a new Pending transfer with the same From/To as an existing
// terminal transfer. Used by the dashboard to re-issue after a timeout or
// failure without forcing the operator to retype the target user. Concurrent
// safety against another in-flight transfer is delegated to
// InitiateExclusive (same TOCTOU-free contract batch-move relies on).
//
// 不在这里对比 s.UserID == prev.FromUserID：
//   - 非 admin 调用方在 controller 层已经被强制为「current.UserID == caller」，
//     所以走到这里时 s.UserID 必然是 caller 自己，不存在静默漂移；
//   - admin 调用方是 last-resort 回收路径，UX 的契约就是「不管 server 现在归
//     谁，把它推给 prev.ToUserID」，被 TestRetryServerTransferAllowsAdmin 钉死。
//     再加一次 FromUserID 校验会把这条 admin 路径拒掉。
func (c *ServerTransferClass) Retry(prev *model.ServerTransfer, initiatorID uint64) (*model.ServerTransfer, error) {
	if !prev.Status.IsTerminal() {
		return nil, fmt.Errorf("cannot retry a non-terminal transfer (status=%d)", prev.Status)
	}
	var s model.Server
	if err := DB.First(&s, prev.ServerID).Error; err != nil {
		return nil, err
	}
	// This must happen before InitiateExclusive because that call flips ownership
	// in the DB; if the target user was deleted, we must fail before any mutation.
	UserLock.RLock()
	_, ok := UserInfoMap[prev.ToUserID]
	UserLock.RUnlock()
	if !ok {
		return nil, fmt.Errorf("target user %d not found", prev.ToUserID)
	}
	if s.UserID == prev.ToUserID {
		return nil, fmt.Errorf("server already belongs to the target user")
	}
	created, err := c.InitiateExclusive(prev.ServerID, s.UserID, prev.ToUserID, initiatorID)
	if err != nil {
		return nil, err
	}
	c.PushIfOnline(created)
	return created, nil
}

// revertTransition is the shared body of MarkFailed/MarkTimeout/Cancel:
// CAS the status, revert Server.UserID, drop from pending index, broadcast.
// Returns the transfer in its post-transition state, or nil if it was no
// longer Pending (silent no-op for idempotency).
//
// In-memory cleanup runs regardless of whether THIS call performed the
// transition. The CAS UPDATE can return RowsAffected=0 because a concurrent
// caller (MarkVerified on the auth path, another revert, the timeout sweep)
// already settled the row between our tx.First and our UPDATE; in that case
// the in-memory pending entry is stale and the auth tolerance window for
// this server has already closed in the DB sense — letting the cache lag
// would keep accepting the old owner's secret for a server that has moved
// on. Self-heal by dropping the in-memory entry whenever the DB shows the
// row as non-Pending.
func (c *ServerTransferClass) revertTransition(transferID uint64, newStatus model.ServerTransferStatus, reason string) (*model.ServerTransfer, error) {
	var t model.ServerTransfer
	// transitionedByThisCall distinguishes "this call performed the CAS"
	// from "row was already terminal before we got here". Both early-return
	// branches MUST leave it false so the post-tx SetUserID(FromUserID)
	// step below is gated on a real Pending → newStatus transition. Without
	// this, OnUsersDeleted + a late Cancel would re-write Server.UserID
	// back to a possibly-deleted FromUserID (regression pinned by
	// TestOnUserDeleteCancelsPendingTransfersAwayFromDeletedUser).
	var transitionedByThisCall bool
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&t, transferID).Error; err != nil {
			return err
		}
		if t.Status != model.ServerTransferStatusPending {
			return nil
		}
		now := time.Now()
		updates := map[string]any{
			"status":     newStatus,
			"updated_at": now,
		}
		if reason != "" {
			updates["last_error"] = reason
		}
		res := tx.Model(&model.ServerTransfer{}).
			Where("id = ? AND status = ?", t.ID, model.ServerTransferStatusPending).
			Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// Concurrent caller won the CAS. Re-read so the outer cleanup
			// observes the authoritative status — otherwise t still holds
			// the Pending snapshot we read at the top and the self-heal
			// below would falsely treat the entry as still Pending.
			return tx.First(&t, transferID).Error
		}
		// As in Initiate: require RowsAffected==1 so a vanished server row
		// aborts the revert instead of silently flipping in-memory state to
		// FromUserID for a row that no longer exists.
		revertRes := tx.Model(&model.Server{}).
			Where("id = ?", t.ServerID).
			Update("user_id", t.FromUserID)
		if revertRes.Error != nil {
			return revertRes.Error
		}
		if revertRes.RowsAffected != 1 {
			return fmt.Errorf("server %d: revert ownership update affected %d rows (want 1) — row likely deleted concurrently", t.ServerID, revertRes.RowsAffected)
		}
		t.Status = newStatus
		t.LastError = reason
		t.UpdatedAt = now
		transitionedByThisCall = true
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Ordering invariant: when this call actually performed the revert
	// (newStatus reached), the in-memory Server.UserID must be reverted
	// to FromUserID BEFORE any other state becomes observable, so auth
	// no longer admits the destination user's global AgentSecret via
	// ServerShared.GetUserID() == userId on the happy "owner match" path.
	if transitionedByThisCall {
		ServerShared.setUserID(t.ServerID, t.FromUserID)
	}

	// Self-heal: any non-Pending DB status invalidates the in-memory entry —
	// but only if that entry is THIS transfer. Without the id check a stale
	// terminal id (e.g. Cancel against a transfer that already failed and
	// has been superseded via Retry by a new Pending row for the same server)
	// would silently wipe the new entry's auth-tolerance window and re-open
	// `HasPending` so a duplicate Initiate could land. cancelServerTransfer
	// does not gate on `t.Status == Pending`, so the stale-id path is
	// reachable from operator UI and replayed API calls; the id match is the
	// only thing keeping the in-memory pending index honest here. The DB row
	// we read (t) is never the live entry's row in that case, so converging
	// the cache to t.Status would be the wrong direction anyway.
	if t.Status != model.ServerTransferStatusPending {
		c.mu.Lock()
		if existing, ok := c.pending[t.ServerID]; ok && existing.ID == t.ID {
			delete(c.pending, t.ServerID)
		}
		c.mu.Unlock()
	}

	// Gate ALL post-tx side effects on transitionedByThisCall, not on
	// `t.Status == newStatus`. A stale terminal-id Cancel against a row
	// that is already Cancelled has t.Status == newStatus too, so the
	// older `!= newStatus` gate let the fall-through re-register the OLD
	// transfer's revertDelivery / terminalSecretRecovery and re-push its
	// RevertHandshakeSecret. After a Retry installed a NEW Pending
	// transfer and delivered its forward HandshakeSecret, the stale
	// rollback supersedes the new credential inside the agent's 10s
	// reload window and strands the new transfer until the 24h timeout
	// sweep. Only the call that actually performed Pending -> newStatus
	// is allowed to drive rollback delivery, recovery registration, stream
	// revocation, broadcast, and push.
	if !transitionedByThisCall {
		return nil, nil
	}
	c.registerRevertDelivery(&t)
	c.registerTerminalSecretRecovery(&t)

	// Ownership rotated back to FromUserID — close any IOStream the
	// destination user opened while they briefly held the server, so the
	// rolled-back FromUserID is not exposed to live sessions from the
	// would-be ToUserID.
	ServerTransferRevokeStreamsForServer(t.ServerID)

	c.broadcast(&t)
	c.pushRevertIfOnline(&t)
	return &t, nil
}

// OnServersDeleted finalizes any in-flight transfers for servers that have
// just been deleted. Without this hook, revertTransition cannot complete
// (its UPDATE on the gone server row fails the RowsAffected==1 invariant
// and aborts), so a Pending row would stay Pending forever, HasPending
// would keep returning true for the doomed server id, and the timeout
// sweeper would log errors every 30s without making progress.
//
// We must NOT touch model.Server here — it is already gone. We CAS each
// Pending row that the listing returned and only invalidate in-memory map
// slots whose (serverID, transferID) match a row we authoritatively
// terminated, so a concurrent Retry that landed a brand-new pending
// transfer in the same slot is not collateral damage.
func (c *ServerTransferClass) OnServersDeleted(serverIDs []uint64) {
	if len(serverIDs) == 0 {
		return
	}

	const reason = "server deleted"
	terminated := make([]model.ServerTransfer, 0, len(serverIDs))
	for _, sid := range serverIDs {
		var pending []model.ServerTransfer
		if err := DB.Where("server_id = ? AND status = ?", sid, model.ServerTransferStatusPending).Find(&pending).Error; err != nil {
			log.Printf("NEZHA>> ServerTransfer OnServersDeleted: list pending for server %d: %v", sid, err)
			continue
		}
		now := time.Now()
		for i := range pending {
			t := pending[i]
			res := DB.Model(&model.ServerTransfer{}).
				Where("id = ? AND status = ?", t.ID, model.ServerTransferStatusPending).
				Updates(map[string]any{
					"status":     model.ServerTransferStatusCancelled,
					"updated_at": now,
					"last_error": reason,
				})
			if res.Error != nil {
				log.Printf("NEZHA>> ServerTransfer OnServersDeleted: cancel transfer %d: %v", t.ID, res.Error)
				continue
			}
			if res.RowsAffected == 0 {
				continue
			}
			t.Status = model.ServerTransferStatusCancelled
			t.LastError = reason
			t.UpdatedAt = now
			terminated = append(terminated, t)
		}
	}

	c.mu.Lock()
	for i := range terminated {
		t := &terminated[i]
		if existing, ok := c.pending[t.ServerID]; ok && existing.ID == t.ID {
			delete(c.pending, t.ServerID)
		}
		if existing, ok := c.revertDeliveries[t.ServerID]; ok && existing.ID == t.ID {
			delete(c.revertDeliveries, t.ServerID)
		}
	}
	// terminalSecretRecovery is keyed by serverID and can outlive an
	// id-matched Cancel/Fail/Timeout that ran before this point — the
	// server itself is gone, so drop unconditionally to prevent a recycled
	// id from inheriting a stale per-transfer credential.
	for _, sid := range serverIDs {
		delete(c.terminalSecretRecovery, sid)
	}
	c.mu.Unlock()

	for _, sid := range serverIDs {
		ServerTransferRevokeStreamsForServer(sid)
	}

	for i := range terminated {
		c.broadcast(&terminated[i])
	}
}

// OnUsersDeleted terminates any Pending transfer whose FromUserID or
// ToUserID is in userIDs, BEFORE the caller drops the corresponding User
// rows. revertTransition's Cancel/Fail/Timeout paths blindly write
// Server.UserID back to FromUserID; if a pending A→B transfer outlives the
// deletion of A, a later timeout sweep (or any Cancel) would silently
// resurrect the deleted user as the server's owner. The same hazard exists
// symmetrically when B is deleted while pending: MarkVerified would promote
// to a nonexistent ToUserID. Settle the row up-front instead, mirroring
// OnServersDeleted's CAS + in-memory cleanup pattern.
//
// We deliberately do NOT touch model.Server here — the live owner may be
// a third party (chained transfers) or the surviving counterparty, and the
// caller's own delete loop (singleton.OnUserDelete) is responsible for any
// servers still attributed to the deleted user.
func (c *ServerTransferClass) OnUsersDeleted(userIDs []uint64) {
	if len(userIDs) == 0 {
		return
	}

	const reason = "user deleted"
	terminated := make([]model.ServerTransfer, 0)
	var pending []model.ServerTransfer
	if err := DB.Where("status = ? AND (from_user_id IN ? OR to_user_id IN ?)",
		model.ServerTransferStatusPending, userIDs, userIDs).Find(&pending).Error; err != nil {
		log.Printf("NEZHA>> ServerTransfer OnUsersDeleted: list pending for users %v: %v", userIDs, err)
		return
	}
	now := time.Now()
	for i := range pending {
		t := pending[i]
		res := DB.Model(&model.ServerTransfer{}).
			Where("id = ? AND status = ?", t.ID, model.ServerTransferStatusPending).
			Updates(map[string]any{
				"status":     model.ServerTransferStatusCancelled,
				"updated_at": now,
				"last_error": reason,
			})
		if res.Error != nil {
			log.Printf("NEZHA>> ServerTransfer OnUsersDeleted: cancel transfer %d: %v", t.ID, res.Error)
			continue
		}
		if res.RowsAffected == 0 {
			continue
		}
		t.Status = model.ServerTransferStatusCancelled
		t.LastError = reason
		t.UpdatedAt = now
		terminated = append(terminated, t)
	}

	c.mu.Lock()
	for i := range terminated {
		t := &terminated[i]
		if existing, ok := c.pending[t.ServerID]; ok && existing.ID == t.ID {
			delete(c.pending, t.ServerID)
		}
		if existing, ok := c.revertDeliveries[t.ServerID]; ok && existing.ID == t.ID {
			delete(c.revertDeliveries, t.ServerID)
		}
		if existing, ok := c.terminalSecretRecovery[t.ServerID]; ok && existing.ID == t.ID {
			delete(c.terminalSecretRecovery, t.ServerID)
		}
	}
	c.mu.Unlock()

	for i := range terminated {
		c.broadcast(&terminated[i])
	}
}

// timeoutSweepLoop is the goroutine started in NewServerTransferClass. It
// wakes every serverTransferTimeoutTickInterval, snapshots the pending index,
// and times out anything older than c.timeout.
func (c *ServerTransferClass) timeoutSweepLoop() {
	ticker := time.NewTicker(serverTransferTimeoutTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.sweepTimeouts()
		}
	}
}

func (c *ServerTransferClass) sweepTimeouts() {
	deadline := time.Now().Add(-c.timeout)

	c.mu.RLock()
	candidates := make([]uint64, 0, len(c.pending))
	for _, t := range c.pending {
		if t.CreatedAt.Before(deadline) {
			candidates = append(candidates, t.ID)
		}
	}
	c.mu.RUnlock()

	// Fan out per-candidate: MarkTimeout's pushRevertIfOnline does a
	// synchronous grpc.ServerStream.Send under the per-server
	// applyConfigSendLock. A single wedged agent would otherwise stall every
	// later candidate in this tick — and because the ticker drops on a busy
	// channel, every subsequent tick too — freezing timeout detection across
	// all tenants. Per-server send ordering is preserved by
	// applyConfigSendLocks; cross-server parallelism is safe. We Wait so the
	// sweep is a synchronous unit, which keeps tests deterministic.
	var wg sync.WaitGroup
	wg.Add(len(candidates))
	for _, id := range candidates {
		id := id
		go func() {
			defer wg.Done()
			_, _ = c.MarkTimeout(id)
		}()
	}
	wg.Wait()
}

// Subscribe registers a channel that will receive every transfer transition
// event from this point forward. The caller MUST Unsubscribe when done or
// the broker will block forever if the channel is unbuffered or full.
func (c *ServerTransferClass) Subscribe() (uint64, <-chan *model.ServerTransfer) {
	c.subMu.Lock()
	defer c.subMu.Unlock()

	id := atomic.AddUint64(&c.nextSubID, 1)
	ch := make(chan *model.ServerTransfer, 16)
	c.subs[id] = ch
	return id, ch
}

func (c *ServerTransferClass) Unsubscribe(id uint64) {
	c.subMu.Lock()
	ch, ok := c.subs[id]
	delete(c.subs, id)
	c.subMu.Unlock()
	if ok {
		close(ch)
	}
}

// broadcast fans the given event out to all subscribers without blocking.
// A subscriber whose buffer is full silently drops the event — the WS layer
// is expected to re-sync via REST when the user revisits a stale view.
func (c *ServerTransferClass) broadcast(t *model.ServerTransfer) {
	snapshot := *t

	c.subMu.Lock()
	defer c.subMu.Unlock()
	for _, ch := range c.subs {
		select {
		case ch <- &snapshot:
		default:
		}
	}
}
