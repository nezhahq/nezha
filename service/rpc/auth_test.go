package rpc

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/metadata"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/service/singleton"
)

// authCheckWithSecret drives (*authHandler).check end-to-end via the same
// gRPC metadata path the real RPC handler uses. Tests rely on it to assert
// what a real reconnect — secret + UUID supplied on the wire — would do.
func authCheckWithSecret(secret, uuid string) (uint64, error) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"client_secret", secret,
		"client_uuid", uuid,
	))
	return (&authHandler{}).Check(ctx)
}

func authCheckWithHyphenatedSecret(secret, uuid string) (uint64, error) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"client-secret", secret,
		"client-uuid", uuid,
	))
	return (&authHandler{}).Check(ctx)
}

// authCheckWithBothKeyStyles reproduces the real post-upgrade wire state: a
// new agent (PR #244) emits BOTH hyphenated and underscore metadata, so a new
// dashboard receives both at once. hyphenSecret/underscoreSecret may differ so
// a test can assert which key wins.
func authCheckWithBothKeyStyles(hyphenSecret, underscoreSecret, uuid string) (uint64, error) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"client-secret", hyphenSecret,
		"client_secret", underscoreSecret,
		"client-uuid", uuid,
		"client_uuid", uuid,
	))
	return (&authHandler{}).Check(ctx)
}

// authHandshakeUUID is RFC4122-shaped so it survives the uuid.ParseUUID gate
// at the top of check(); setupAuthAgentFixture's "uuid-alice" / "uuid-bob"
// only work for callers that bypass check() and exercise the inner helpers.
const authHandshakeUUID = "11111111-1111-1111-1111-111111111111"

// setupAuthHandshakeFixture seeds a single server (id=11, owner=user 100,
// real UUID) plus the user-secret tables so the global-secret fall-through
// in check() has something to match. Mirrors setupAuthAgentFixture's reset
// discipline but additionally restores AgentSecretToUserId / UserInfoMap.
func setupAuthHandshakeFixture(t *testing.T) func() {
	t.Helper()
	originalDB := singleton.DB
	originalServerShared := singleton.ServerShared
	originalServerTransferShared := singleton.ServerTransferShared
	originalUserInfoMap := singleton.UserInfoMap
	originalAgentSecretToUserId := singleton.AgentSecretToUserId

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.Server{}, &model.ServerTransfer{}, &model.WAF{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&model.Server{
		Common: model.Common{ID: 11, UserID: 100},
		UUID:   authHandshakeUUID,
		Name:   "handshake-srv",
	}).Error; err != nil {
		t.Fatalf("create handshake server: %v", err)
	}
	singleton.DB = db
	singleton.ServerShared = singleton.NewServerClass()
	srv := &model.Server{Common: model.Common{ID: 11, UserID: 100}, UUID: authHandshakeUUID, Name: "handshake-srv"}
	model.InitServer(srv)
	singleton.ServerShared.Update(srv, authHandshakeUUID)
	singleton.ServerTransferShared = singleton.NewServerTransferClass()

	singleton.UserLock.Lock()
	singleton.UserInfoMap = map[uint64]model.UserInfo{
		100: {Role: model.RoleMember, AgentSecret: "alice-global"},
		200: {Role: model.RoleMember, AgentSecret: "bob-global"},
	}
	singleton.AgentSecretToUserId = map[string]uint64{
		"alice-global": 100,
		"bob-global":   200,
	}
	singleton.UserLock.Unlock()

	return func() {
		if singleton.ServerTransferShared != nil {
			singleton.ServerTransferShared.Stop()
		}
		singleton.DB = originalDB
		singleton.ServerShared = originalServerShared
		singleton.ServerTransferShared = originalServerTransferShared
		singleton.UserLock.Lock()
		singleton.UserInfoMap = originalUserInfoMap
		singleton.AgentSecretToUserId = originalAgentSecretToUserId
		singleton.UserLock.Unlock()
	}
}

func TestAuthCheckAcceptsHyphenatedMetadata(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()

	cid, err := authCheckWithHyphenatedSecret("alice-global", authHandshakeUUID)
	if err != nil {
		t.Fatalf("hyphenated metadata must authenticate: %v", err)
	}
	if cid != 11 {
		t.Fatalf("expected server ID 11, got %d", cid)
	}
}

// The everyday post-upgrade case (new agent + new dashboard): both key styles
// arrive together carrying the same secret and must authenticate normally.
func TestAuthCheckAcceptsBothKeyStylesPresent(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()

	cid, err := authCheckWithBothKeyStyles("alice-global", "alice-global", authHandshakeUUID)
	if err != nil {
		t.Fatalf("an agent emitting both key styles must authenticate: %v", err)
	}
	if cid != 11 {
		t.Fatalf("expected server ID 11, got %d", cid)
	}
}

// When both styles are present the hyphenated key wins (firstMetadataValue
// lists it first). This pins the precedence so a future reorder can't silently
// start trusting the underscore alias that Caddy strips.
func TestAuthCheckHyphenatedKeyTakesPrecedence(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()

	cid, err := authCheckWithBothKeyStyles("alice-global", "garbage-underscore", authHandshakeUUID)
	if err != nil {
		t.Fatalf("hyphenated secret must be the one used, so auth must succeed: %v", err)
	}
	if cid != 11 {
		t.Fatalf("expected server ID 11 from the hyphenated secret, got %d", cid)
	}
}

// setupAuthAgentFixture seeds an in-memory DB and ServerShared with two
// servers belonging to different users so we can assert that a secret bound
// to user A cannot resolve a server UUID owned by user B.
func setupAuthAgentFixture(t *testing.T) func() {
	t.Helper()
	originalDB := singleton.DB
	originalServerShared := singleton.ServerShared
	originalServerTransferShared := singleton.ServerTransferShared

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.Server{}, &model.ServerTransfer{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&model.Server{
		Common: model.Common{ID: 1, UserID: 100},
		UUID:   "uuid-alice",
		Name:   "alice-srv",
	}).Error; err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if err := db.Create(&model.Server{
		Common: model.Common{ID: 2, UserID: 200},
		UUID:   "uuid-bob",
		Name:   "bob-srv",
	}).Error; err != nil {
		t.Fatalf("create bob: %v", err)
	}
	singleton.DB = db
	singleton.ServerShared = singleton.NewServerClass()
	singleton.ServerTransferShared = singleton.NewServerTransferClass()

	return func() {
		if singleton.ServerTransferShared != nil {
			singleton.ServerTransferShared.Stop()
		}
		singleton.DB = originalDB
		singleton.ServerShared = originalServerShared
		singleton.ServerTransferShared = originalServerTransferShared
	}
}

func TestAuthorizeAgentForUUIDAcceptsOwnedServer(t *testing.T) {
	defer setupAuthAgentFixture(t)()

	cid, hasID, err := authorizeAgentForUUID(100, "uuid-alice")
	if err != nil {
		t.Fatalf("alice with her own server UUID must not error, got %v", err)
	}
	if !hasID || cid != 1 {
		t.Fatalf("expected (cid=1, hasID=true), got (cid=%d, hasID=%v)", cid, hasID)
	}
}

// Core regression: an agent presenting user A's secret but user B's server
// UUID must be rejected. Previously the code returned the resolved server ID
// without verifying the UserID matched the secret owner, allowing same-tenant
// (and worse — cross-tenant if UUID leaks) server impersonation.
func TestAuthorizeAgentForUUIDRejectsForeignServerUUID(t *testing.T) {
	defer setupAuthAgentFixture(t)()

	_, _, err := authorizeAgentForUUID(100, "uuid-bob") // alice's secret + bob's UUID
	if err == nil {
		t.Fatalf("UUID owned by another user must be rejected")
	}
}

func TestAuthorizeAgentForUUIDAllowsGlobalDefaultSecret(t *testing.T) {
	defer setupAuthAgentFixture(t)()

	cid, hasID, err := authorizeAgentForUUID(0, "uuid-bob")
	if err != nil {
		t.Fatalf("global default secret must be allowed to use existing UUIDs, got %v", err)
	}
	if !hasID || cid != 2 {
		t.Fatalf("expected (cid=2, hasID=true), got (cid=%d, hasID=%v)", cid, hasID)
	}
}

// An unknown UUID must NOT be treated as an impersonation attempt — it is
// the normal first-time registration path and the caller (Check) creates a
// new server bound to the secret owner.
func TestAuthorizeAgentForUUIDPermitsUnknownUUIDForRegistration(t *testing.T) {
	defer setupAuthAgentFixture(t)()

	cid, hasID, err := authorizeAgentForUUID(100, "uuid-never-seen-before")
	if err != nil {
		t.Fatalf("unknown UUID must be permitted for new registration, got %v", err)
	}
	if hasID {
		t.Fatalf("hasID must be false for unknown UUID, got cid=%d", cid)
	}
}

// initiatePendingTransfer mirrors the controller flow used by the batch-move
// endpoint to drive ownership through ServerTransferShared. Tests use it to
// set up the auth-tolerance window with the Server row already flipped to
// ToUserID. Returns nothing; callers use ServerTransferShared.LookupPending
// to fetch the row if they need it.
func initiatePendingTransfer(t *testing.T, serverID, fromUserID, toUserID uint64) {
	t.Helper()
	var created *model.ServerTransfer
	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		created, err = singleton.ServerTransferShared.Initiate(tx, serverID, fromUserID, toUserID, fromUserID)
		return err
	})
	if err != nil {
		t.Fatalf("initiate pending transfer: %v", err)
	}
	singleton.ServerTransferShared.Register(created)
}

// The auth-tolerance window: while a Pending transfer exists for this server,
// the old owner's AgentSecret must still authenticate this UUID — the agent
// hasn't received the new secret yet via ApplyConfig. Without this, every
// in-flight transfer would knock the affected agent offline immediately.
func TestAuthorizeAgentForUUIDAcceptsFromUserDuringPendingTransfer(t *testing.T) {
	defer setupAuthAgentFixture(t)()

	// Alice initiates: server 1 moves from alice (100) to bob (200).
	// Server.UserID is now 200; alice's agent still presents secret==100.
	initiatePendingTransfer(t, 1, 100, 200)

	cid, hasID, err := authorizeAgentForUUID(100, "uuid-alice")
	if err != nil {
		t.Fatalf("FromUserID secret must be accepted during pending window, got %v", err)
	}
	if !hasID || cid != 1 {
		t.Fatalf("expected (cid=1, hasID=true), got (cid=%d, hasID=%v)", cid, hasID)
	}
}

// Tolerance is narrowly scoped: an unrelated user's secret must NOT be
// accepted just because *some* transfer is in flight. Specifically, only
// secrets matching FromUserID or ToUserID get through.
func TestAuthorizeAgentForUUIDRejectsThirdPartyDuringPendingTransfer(t *testing.T) {
	defer setupAuthAgentFixture(t)()
	initiatePendingTransfer(t, 1, 100, 200)

	// userId=999 has nothing to do with this transfer.
	_, _, err := authorizeAgentForUUID(999, "uuid-alice")
	if err == nil {
		t.Fatalf("third-party secret must be rejected even while a transfer is pending")
	}
}

// SECURITY: during a Pending transfer the destination user's user-global
// AgentSecret must NOT close the pending window. PushIfOnline only delivers
// the per-transfer HandshakeSecret on the wire, so a reconnect under the
// destination user's global AgentSecret is not proof of agent rotation —
// it could just be the destination user authenticating with their own
// secret + the now-visible Server.UUID. Reject it; only the per-transfer
// HandshakeSecret path may promote to Verified.
func TestAuthorizeAgentForUUIDRejectsToUserGlobalSecretDuringPendingTransfer(t *testing.T) {
	defer setupAuthAgentFixture(t)()
	initiatePendingTransfer(t, 1, 100, 200)

	if _, _, err := authorizeAgentForUUID(200, "uuid-alice"); err == nil {
		t.Fatal("destination user's global AgentSecret must NOT authenticate during pending transfer; only per-transfer HandshakeSecret may close the window")
	}
	if !singleton.ServerTransferShared.HasPending(1) {
		t.Fatal("pending transfer must survive a destination-user global AgentSecret reconnect")
	}

	if _, _, err := authorizeAgentForUUID(100, "uuid-alice"); err != nil {
		t.Fatalf("FromUser tolerance window must remain open while transfer is still Pending, got %v", err)
	}
}

// During the revert recovery window the destination user's global
// AgentSecret must NOT be accepted by authorizeAgentForUUID. PushIfOnline
// only delivers per-transfer HandshakeSecret / RevertHandshakeSecret on
// the wire, so a reconnect under the ToUserID global secret cannot come
// from the real agent — it can only come from the destination user
// themselves, who can see Server.UUID and would otherwise impersonate the
// agent during rollback, trigger pushRevertIfOnline to leak
// RevertHandshakeSecret, and get promoted via MarkRevertDelivered.
// Legitimate recovery goes through FromUserID's global secret, the
// forward HandshakeSecret, or the RevertHandshakeSecret.
func TestAuthorizeAgentForUUIDRejectsToUserGlobalSecretDuringRevertRecovery(t *testing.T) {
	defer setupAuthAgentFixture(t)()
	initiatePendingTransfer(t, 1, 100, 200)
	pending, ok := singleton.ServerTransferShared.LookupPending(1)
	if !ok {
		t.Fatal("expected pending transfer")
	}
	if _, err := singleton.ServerTransferShared.Cancel(pending.ID); err != nil {
		t.Fatalf("cancel transfer: %v", err)
	}

	if _, _, err := authorizeAgentForUUID(200, "uuid-alice"); err == nil {
		t.Fatal("destination user's global AgentSecret must NOT authenticate during revert recovery; only per-transfer HandshakeSecret / RevertHandshakeSecret may close the window")
	}
	if _, ok := singleton.ServerTransferShared.LookupRevertDelivery(1); !ok {
		t.Fatal("rejected ToUserID auth must not consume the revert delivery — the real agent still needs it for the eventual per-transfer recovery")
	}
}

// Regression for finding A: after MarkVerified deletes the pending entry,
// the agent's persisted ClientSecret is still the per-transfer
// HandshakeSecret (PushIfOnline only ever delivered that value). The very
// next reconnect — gRPC stream drop, agent restart, network blip — must
// keep authenticating, otherwise the agent silently locks itself out on
// the now-orphaned handshake token. There is no follow-up ApplyConfig
// path that swaps the agent over to the destination user's stable
// AgentSecret, so auth itself has to keep treating the post-Verified
// HandshakeSecret as a valid credential for that server.
func TestAuthHandshakeSecretStillAuthenticatesAfterMarkVerified(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()

	initiatePendingTransfer(t, 11, 100, 200)
	pending, ok := singleton.ServerTransferShared.LookupPending(11)
	if !ok {
		t.Fatal("expected pending transfer")
	}
	handshakeSecret := pending.HandshakeSecret
	if handshakeSecret == "" {
		t.Fatal("precondition: pending transfer must carry a HandshakeSecret")
	}

	cid, err := authCheckWithSecret(handshakeSecret, authHandshakeUUID)
	if err != nil {
		t.Fatalf("first reconnect with HandshakeSecret must promote the transfer, got %v", err)
	}
	if cid != 11 {
		t.Fatalf("first reconnect must resolve to server 11, got %d", cid)
	}
	if singleton.ServerTransferShared.HasPending(11) {
		t.Fatal("MarkVerified must have cleared the pending index after the handshake reconnect")
	}

	if _, err := authCheckWithSecret(handshakeSecret, authHandshakeUUID); err != nil {
		t.Fatalf("second reconnect with the same HandshakeSecret must still authenticate (the agent has no other credential to present until a final hand-off completes); got %v", err)
	}
}

// First successful auth with RevertHandshakeSecret proves the agent has
// applied the rollback (10s reload + applyPendingReload have committed
// the secret to disk). At that point the auth path must promote the
// secret into the long-term verifiedHandshakes map and consume the
// temporary revertDeliveries entry — otherwise the only acceptance path
// is LookupByRevertHandshakeSecret, which prunes after
// defaultRevertDeliveryRecoveryWindow and leaves the agent locked out
// ~24h later. See ServerTransferClass.MarkRevertDelivered.
func TestAuthRevertHandshakeSecretPromotesToVerifiedAndKeepsAuthenticating(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()

	initiatePendingTransfer(t, 11, 100, 200)
	pending, ok := singleton.ServerTransferShared.LookupPending(11)
	if !ok {
		t.Fatal("expected pending transfer")
	}
	if _, err := singleton.ServerTransferShared.Cancel(pending.ID); err != nil {
		t.Fatalf("cancel transfer to register a revert delivery: %v", err)
	}
	revert, ok := singleton.ServerTransferShared.LookupRevertDelivery(11)
	if !ok {
		t.Fatal("precondition: cancel must have registered a revert delivery")
	}
	revertHandshake := revert.RevertHandshakeSecret
	if revertHandshake == "" {
		t.Fatal("precondition: revert delivery must carry a RevertHandshakeSecret")
	}

	if _, err := authCheckWithSecret(revertHandshake, authHandshakeUUID); err != nil {
		t.Fatalf("first auth with RevertHandshakeSecret must succeed, got %v", err)
	}

	if _, ok := singleton.ServerTransferShared.LookupRevertDelivery(11); ok {
		t.Fatal("first successful auth must consume the temporary revertDelivery — the credential is now promoted to the long-term map")
	}

	sid, ok := singleton.ServerTransferShared.LookupServerByVerifiedHandshakeSecret(revertHandshake)
	if !ok || sid != 11 {
		t.Fatalf("RevertHandshakeSecret must be promoted into verifiedHandshakes; lookup got (sid=%d, ok=%v)", sid, ok)
	}

	if _, err := authCheckWithSecret(revertHandshake, authHandshakeUUID); err != nil {
		t.Fatalf("second auth via the promoted verifiedHandshakes path must still succeed, got %v", err)
	}
}

// HIGH security regression: if a transfer has already been Cancelled/Failed/
// Timed out, its HandshakeSecret must NEVER authenticate. Today auth.check
// calls MarkVerified on the lookup result and treats RowsAffected==0 as
// success, so an attacker who learned the per-transfer HandshakeSecret
// (e.g. previous owner whose stream was hijacked during Pending) can
// authenticate inside the narrow race window where revertTransition has
// changed DB status but not yet deleted the in-memory pending entry, or
// after that window simply because the swallowed return is `return
// t.ServerID, nil`.
//
// Expected: when the transfer row is no longer Pending, auth must reject
// the HandshakeSecret entirely.
func TestAuthHandshakeSecretRejectedAfterTransferTerminated(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()

	initiatePendingTransfer(t, 11, 100, 200)
	pending, ok := singleton.ServerTransferShared.LookupPending(11)
	if !ok {
		t.Fatal("expected pending transfer")
	}
	handshakeSecret := pending.HandshakeSecret

	// Settle the DB row to Cancelled WITHOUT touching the in-memory
	// pending entry. This reproduces the race window in revertTransition
	// between the DB CAS and the c.mu.Lock that deletes the pending
	// entry; LookupByHandshakeSecret still hits.
	if err := singleton.DB.Model(&model.ServerTransfer{}).
		Where("id = ?", pending.ID).
		Update("status", model.ServerTransferStatusCancelled).Error; err != nil {
		t.Fatalf("simulate concurrent cancel: %v", err)
	}

	_, err := authCheckWithSecret(handshakeSecret, authHandshakeUUID)
	if err == nil {
		t.Fatal("HandshakeSecret on a terminated transfer must be rejected — auth swallowed MarkVerified RowsAffected==0 and returned success, enabling auth bypass with a stale per-transfer secret")
	}
}

// HIGH security regression: the auth tolerance window for the old owner's
// global AgentSecret must close in lockstep with MarkVerified. Holding c.mu
// across the DB CAS, the c.pending delete and the verifiedHandshakes write
// inside MarkVerified makes those three steps a single observable event for
// any auth-path lookup taking c.mu.RLock; once MarkVerified returns
// verified=true, no later authorizeAgentForUUID can still see the pending
// entry that previously admitted FromUserID.
func TestAuthOldOwnerSecretRejectedOnceTransferIsVerifiedInDB(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()

	initiatePendingTransfer(t, 11, 100, 200)
	pending, ok := singleton.ServerTransferShared.LookupPending(11)
	if !ok {
		t.Fatal("expected pending transfer")
	}

	verified, _, err := singleton.ServerTransferShared.MarkVerified(11, pending.ID)
	if err != nil {
		t.Fatalf("MarkVerified must succeed for a fresh pending: %v", err)
	}
	if !verified {
		t.Fatal("MarkVerified must report verified=true for a fresh pending")
	}

	if _, _, err := authorizeAgentForUUID(100, authHandshakeUUID); err == nil {
		t.Fatal("old owner's global AgentSecret must be rejected once MarkVerified has returned — the auth tolerance window must not outlive the verified transition")
	}
}

// FORWARD-RECOVERY (HIGH): symmetric to TestAuthHandshakeSecretRejectedAfter
// TransferTerminated. That test pokes the DB directly to simulate an
// attacker who learned the per-transfer forward HandshakeSecret outside
// of any dashboard-driven cancellation; auth must reject. This test
// exercises the OTHER scenario: a legitimate agent that already wrote the
// forward HandshakeSecret to disk via the 10s reload timer, and the
// dashboard cancels the transfer via the normal Cancel API (which goes
// through revertTransition). The agent's next reconnect presents the
// forward HandshakeSecret. Auth must authenticate it so RequestTask can
// run OnAgentReconnect and push the RevertHandshakeSecret rollback —
// otherwise the agent is permanently locked out and the operator has to
// SSH in and edit the config by hand.
//
// The distinguishing signal is whether revertTransition was the one that
// settled the row: it populates terminalForwardRecovery; a direct DB
// poke does not.
func TestAuthForwardHandshakeSecretAcceptedAfterDashboardCancel(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()

	initiatePendingTransfer(t, 11, 100, 200)
	pending, ok := singleton.ServerTransferShared.LookupPending(11)
	if !ok {
		t.Fatal("expected pending transfer")
	}
	forward := pending.HandshakeSecret

	if _, err := singleton.ServerTransferShared.Cancel(pending.ID); err != nil {
		t.Fatalf("dashboard Cancel must succeed: %v", err)
	}

	cid, err := authCheckWithSecret(forward, authHandshakeUUID)
	if err != nil {
		t.Fatalf("forward HandshakeSecret must authenticate after dashboard Cancel so RequestTask can deliver the rollback; got %v", err)
	}
	if cid != 11 {
		t.Fatalf("forward HandshakeSecret must resolve to its bound server, got cid=%d", cid)
	}

	if _, ok := singleton.ServerTransferShared.LookupServerByVerifiedHandshakeSecret(forward); ok {
		t.Fatal("forward HandshakeSecret on a terminated transfer must NOT be promoted into verifiedHandshakes — promotion would outlive the bounded recovery window and turn a cancelled credential into a permanent one")
	}
}

// wafAgentAuthFailCount returns the recorded WAF count for the given IP +
// gRPC block identifier. Used by the bad-credential WAF tests to assert
// FirstOrCreate / UPDATE actually fired.
func wafAgentAuthFailCount(t *testing.T, ip string) uint64 {
	t.Helper()
	bin, err := utils.IPStringToBinary(ip)
	if err != nil {
		t.Fatalf("ip parse: %v", err)
	}
	var w model.WAF
	res := singleton.DB.Where("ip = ? AND block_identifier = ?", bin, model.BlockIDgRPC).First(&w)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return 0
		}
		t.Fatalf("query waf: %v", res.Error)
	}
	return w.Count
}

// authCheckFromIP feeds an attacker IP through the real Check entry point
// so the WAF BlockIP path observes a non-empty CtxKeyRealIP. authCheckWithSecret
// uses a bare context.Background which keeps the IP empty and short-circuits
// BlockIP(ip == ""), masking the very regression these tests want to pin.
func authCheckFromIP(secret, uuid, ip string) (uint64, error) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"client_secret", secret,
		"client_uuid", uuid,
	))
	ctx = context.WithValue(ctx, model.CtxKeyRealIP{}, ip)
	return (&authHandler{}).Check(ctx)
}

// REGRESSION: the new per-transfer handshake path moved the client_uuid
// validation in front of the global AgentSecretToUserId lookup. A bad
// secret paired with a malformed/missing UUID now short-circuits to
// "客户端 UUID 不合法" and skips the BlockIP(WAFBlockReasonTypeAgentAuthFail)
// counter the previous implementation incremented. That counter is the
// only thing throttling brute-force on agent secrets — losing it lets an
// attacker enumerate secrets indefinitely just by also corrupting the
// UUID metadata. Both the missing-secret and bad-secret cases must still
// count toward AgentAuthFail when the UUID is unusable.
func TestAuthBadSecretInvalidUUIDStillIncrementsAgentAuthFailWAF(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()
	const attackerIP = "203.0.113.7"

	if _, err := authCheckFromIP("definitely-not-a-real-secret", "not-a-uuid", attackerIP); err == nil {
		t.Fatal("Check must reject bogus credentials")
	}

	if got := wafAgentAuthFailCount(t, attackerIP); got == 0 {
		t.Fatalf("bad client_secret + invalid client_uuid must still count toward WAFBlockReasonTypeAgentAuthFail; got count=%d", got)
	}
}

// Mirror of the above for the empty-UUID metadata path. uuid.ParseUUID("")
// also errors out, so the same auth-fail counting must apply — otherwise an
// attacker can just omit the metadata key entirely.
func TestAuthBadSecretEmptyUUIDStillIncrementsAgentAuthFailWAF(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()
	const attackerIP = "203.0.113.8"

	if _, err := authCheckFromIP("another-bad-secret", "", attackerIP); err == nil {
		t.Fatal("Check must reject bogus credentials")
	}

	if got := wafAgentAuthFailCount(t, attackerIP); got == 0 {
		t.Fatalf("bad client_secret + empty client_uuid must still count toward WAFBlockReasonTypeAgentAuthFail; got count=%d", got)
	}
}

// FORWARD-RECOVERY: forward secret bound to server A must not authenticate
// when presented with server B's UUID. Defence against an attacker who
// learns one server's forward secret and tries to attach it to a different
// agent during the recovery window.
func TestAuthForwardHandshakeSecretRejectedForDifferentUUID(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()

	initiatePendingTransfer(t, 11, 100, 200)
	pending, ok := singleton.ServerTransferShared.LookupPending(11)
	if !ok {
		t.Fatal("expected pending transfer")
	}
	forward := pending.HandshakeSecret

	if _, err := singleton.ServerTransferShared.Cancel(pending.ID); err != nil {
		t.Fatalf("dashboard Cancel must succeed: %v", err)
	}

	const otherUUID = "22222222-2222-2222-2222-222222222222"
	if _, err := authCheckWithSecret(forward, otherUUID); err == nil {
		t.Fatal("forward HandshakeSecret must be rejected when paired with a different server UUID even during recovery — token is per-(server, transfer)")
	}
}

// SECURITY (P1): PushIfOnline only ever delivers the per-transfer
// HandshakeSecret to the real agent; the destination user's global
// AgentSecret is never sent on the wire and is therefore not proof of
// agent rotation. Server.UUID is visible to the destination user once
// Register flips Server.UserID, so admitting (ToUser global secret, real
// UUID) and calling MarkVerified would let the destination user clear
// the auth tolerance window for FromUser's secret (locking the real
// agent out) and flip transfer state to Verified without the agent ever
// applying the new credential. Only LookupByHandshakeSecret may promote.
func TestAuthDestinationUserGlobalSecretDoesNotVerifyPendingTransfer(t *testing.T) {
	defer setupAuthHandshakeFixture(t)()

	initiatePendingTransfer(t, 11, 100, 200)
	if !singleton.ServerTransferShared.HasPending(11) {
		t.Fatal("precondition: pending transfer must be registered")
	}

	cid, err := authCheckWithSecret("bob-global", authHandshakeUUID)
	if err == nil {
		t.Fatalf("destination user's global AgentSecret must not close the transfer's pending window; got cid=%d", cid)
	}

	if !singleton.ServerTransferShared.HasPending(11) {
		t.Fatal("pending transfer must survive a destination-user global AgentSecret reconnect; only the per-transfer HandshakeSecret may promote to Verified")
	}
}
