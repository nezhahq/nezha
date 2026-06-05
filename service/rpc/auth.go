package rpc

import (
	"context"
	"fmt"
	"log"
	"strings"

	petname "github.com/dustinkirkland/golang-petname"
	"github.com/hashicorp/go-uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

type authHandler struct {
	ClientSecret string
	ClientUUID   string
}

func (a *authHandler) Check(ctx context.Context) (uint64, error) {
	return a.check(ctx)
}

func (a *authHandler) CheckRequestTask(ctx context.Context) (uint64, error) {
	return a.check(ctx)
}

// 所有 auth caller 走完全相同的 ServerTransfer dual-secret 容忍策略。
// revertDelivery 不在 auth 阶段消费 —— 真正派发 rollback ApplyConfig 的
// pushRevertIfOnline 才有资格清理它，否则 auth 提前清就会让 OnAgentReconnect
// 找不到 recovery 记录，agent 10s timer 一到就锁死在被拒绝的新 secret 上。
func (a *authHandler) check(ctx context.Context) (uint64, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return 0, status.Errorf(codes.Unauthenticated, "获取 metaData 失败")
	}

	clientSecret := firstMetadataValue(md, "client-secret", "client_secret")

	if clientSecret == "" {
		return 0, status.Error(codes.Unauthenticated, "客户端认证失败")
	}

	ip, _ := ctx.Value(model.CtxKeyRealIP{}).(string)

	clientUUID := firstMetadataValue(md, "client-uuid", "client_uuid")

	if _, err := uuid.ParseUUID(clientUUID); err != nil {
		// Keep this counter on the same trigger surface as the
		// unknown-secret path below: an attacker who pairs a bad secret
		// with a malformed/missing UUID otherwise bypasses
		// WAFBlockReasonTypeAgentAuthFail entirely and gets unbounded
		// retries (TestAuthBadSecret*InvalidUUIDStillIncrementsAgentAuthFailWAF).
		model.BlockIP(singleton.DB, ip, model.WAFBlockReasonTypeAgentAuthFail, model.BlockIDgRPC)
		return 0, status.Error(codes.Unauthenticated, "客户端 UUID 不合法")
	}

	// Per-transfer handshake secret path: ApplyConfig delivers a random
	// per-transfer token instead of the destination user's global AgentSecret
	// (see PushIfOnline). When the agent reconnects under that token the auth
	// layer recognises it here, scoped to the matching server UUID, and
	// promotes the transfer to Verified. The user-global secret lookup below
	// continues to handle every non-transfer agent, plus the still-tolerated
	// previous-owner secret during the Pending window. Checked before the
	// global lookup so the handshake-secret token can never collide with
	// some other user's accidental match.
	if singleton.ServerTransferShared != nil {
		if t, ok := singleton.ServerTransferShared.LookupByHandshakeSecret(clientSecret); ok {
			cid, found := singleton.ServerShared.UUIDToID(clientUUID)
			if !found || cid != t.ServerID {
				return 0, status.Error(codes.Unauthenticated, "transfer handshake secret bound to a different server")
			}
			// Auth via per-transfer HandshakeSecret succeeds only when
			// MarkVerified actually performs the Pending → Verified
			// transition. A lost CAS (concurrent Cancel/Fail/Timeout)
			// means the credential is stale; the verifiedHandshakes
			// fallthrough below will still admit it if it had been
			// promoted by a successful previous reconnect, otherwise it
			// is rejected.
			verified, _, err := singleton.ServerTransferShared.MarkVerified(t.ServerID, t.ID)
			if err != nil {
				log.Printf("NEZHA>> ServerTransfer MarkVerified(cid=%d) via handshake secret failed: %v", t.ServerID, err)
				return 0, status.Error(codes.Unauthenticated, "transfer handshake verification failed")
			}
			if verified {
				model.UnblockIP(singleton.DB, ip, model.BlockIDgRPC)
				return t.ServerID, nil
			}
		}
		// Bounded terminal-recovery window: a transfer was Cancel/Fail/
		// Timeout-ed and the agent may still be presenting either of its
		// per-transfer secrets. Single lookup + kind switch:
		//
		//   forward — agent committed t.HandshakeSecret to disk before
		//             the dashboard observed MarkVerified. Admit so
		//             RequestTask → OnAgentReconnect can deliver the
		//             rollback ApplyConfig. DO NOT call MarkVerified
		//             (transfer is terminal) and DO NOT promote into
		//             verifiedHandshakes (the agent's stable post-rollback
		//             credential will be the revert secret, not this one).
		//
		//   revert  — agent has applied the rollback and presented
		//             t.RevertHandshakeSecret. Promote via
		//             MarkRevertDelivered so the credential survives
		//             past the recovery window (~24h sweep).
		//
		// SECURITY: terminalSecretRecovery is only populated by
		// revertTransition. A stolen per-transfer secret on a transfer
		// whose terminal status was forged in the DB never reaches this
		// table — TestAuthHandshakeSecretRejectedAfterTransferTerminated
		// pins that path closed.
		if t, kind, ok := singleton.ServerTransferShared.LookupByTerminalSecretRecovery(clientSecret); ok {
			cid, found := singleton.ServerShared.UUIDToID(clientUUID)
			if !found || cid != t.ServerID {
				return 0, status.Error(codes.Unauthenticated, "transfer terminal-recovery secret bound to a different server")
			}
			model.UnblockIP(singleton.DB, ip, model.BlockIDgRPC)
			if kind == singleton.TerminalRecoveryRevert {
				if err := singleton.ServerTransferShared.MarkRevertDelivered(t.ServerID, t.ID); err != nil {
					log.Printf("NEZHA>> ServerTransfer MarkRevertDelivered(server=%d transfer=%d) failed: %v", t.ServerID, t.ID, err)
				}
			}
			return t.ServerID, nil
		}
		// Post-MarkVerified path: the agent's persisted client_secret is
		// the per-transfer HandshakeSecret (PushIfOnline never delivers a
		// user-global secret), and no follow-up ApplyConfig swaps it back
		// out. So every reconnect after the first one — stream drop, agent
		// restart, etc. — must still match this credential, bound strictly
		// to (serverID, UUID). The match is constrained to a single server
		// because the handshake secret was generated per-transfer; it does
		// not unlock any other agent. A new transfer for the same server
		// invalidates the entry inside Register, closing this acceptance
		// window before the next HandshakeSecret takes over.
		if cid, ok := singleton.ServerTransferShared.LookupServerByVerifiedHandshakeSecret(clientSecret); ok {
			if uuidCID, found := singleton.ServerShared.UUIDToID(clientUUID); found && uuidCID == cid {
				model.UnblockIP(singleton.DB, ip, model.BlockIDgRPC)
				return cid, nil
			}
			return 0, status.Error(codes.Unauthenticated, "transfer verified handshake secret bound to a different server")
		}
	}

	singleton.UserLock.RLock()
	userId, ok := singleton.AgentSecretToUserId[clientSecret]
	if !ok {
		singleton.UserLock.RUnlock()
		model.BlockIP(singleton.DB, ip, model.WAFBlockReasonTypeAgentAuthFail, model.BlockIDgRPC)
		return 0, status.Error(codes.Unauthenticated, "客户端认证失败")
	}
	singleton.UserLock.RUnlock()

	model.UnblockIP(singleton.DB, ip, model.BlockIDgRPC)

	clientID, hasID, err := authorizeAgentForUUID(userId, clientUUID)
	if err != nil {
		return 0, status.Error(codes.Unauthenticated, err.Error())
	}
	if !hasID {
		s := model.Server{UUID: clientUUID, Name: petname.Generate(2, "-"), Common: model.Common{
			UserID: userId,
		}}
		if err := singleton.DB.Create(&s).Error; err != nil {
			return 0, status.Error(codes.Unauthenticated, err.Error())
		}

		model.InitServer(&s)
		singleton.ServerShared.Update(&s, clientUUID)

		clientID = s.ID
	}

	return clientID, nil
}

func firstMetadataValue(md metadata.MD, keys ...string) string {
	for _, key := range keys {
		if value, ok := md[key]; ok && len(value) > 0 {
			return strings.TrimSpace(value[0])
		}
	}
	return ""
}

// authorizeAgentForUUID resolves a client UUID to the dashboard's internal
// server ID, ensuring the resolved server is actually owned by the agent
// secret's owner. Previously Check returned the resolved server ID without
// verifying ownership, allowing an agent that knew another user's server
// UUID to impersonate it (poisoning monitoring state, triggering alerts).
// hasID=false means the UUID is unknown and the caller may register it as
// a new server for the secret owner.
//
// The error path also doubles as a leak-detection signal for operators: if
// an agent persistently fails with "client UUID does not belong to the
// agent secret owner", it pins down which user's secret has been reused
// against a server they don't own.
//
// Server transfer interaction: while a ServerTransfer is Pending for this
// server, the agent is still authenticating with the previous owner's
// AgentSecret (the new secret has not yet propagated). To keep that agent
// online during the rollover, accept userId==FromUserID for the duration of
// the pending window. The dual-secret tolerance is narrowly scoped to the
// affected server only — every other agent of either user is unaffected.
// Once the agent reconnects under the new owner's secret (userId==ToUserID
// matching server.UserID), MarkVerified promotes the transfer and closes
// the tolerance window.
func authorizeAgentForUUID(userId uint64, clientUUID string) (clientID uint64, hasID bool, err error) {
	cid, found := singleton.ServerShared.UUIDToID(clientUUID)
	if !found {
		return 0, false, nil
	}
	server, _ := singleton.ServerShared.Get(cid)
	if server == nil {
		// Cache inconsistency: UUID maps to an ID, but no server record exists.
		// Treat as unknown (registration path) rather than impersonation.
		return 0, false, nil
	}
	if userId == 0 {
		// The legacy global agent secret maps to user 0. It predates per-user
		// agent secrets, so keep it compatible by allowing any existing UUID.
		return cid, true, nil
	}
	if server.GetUserID() == userId {
		// SECURITY: while a transfer is Pending, Server.UserID has already
		// been flipped to ToUserID by Register, so userId==Server.UserID
		// here also matches the destination user's user-global AgentSecret.
		// PushIfOnline only delivers the per-transfer HandshakeSecret on
		// the wire; the destination user's global AgentSecret is never
		// pushed to the agent, so a reconnect under that secret is not
		// proof of agent rotation. Admitting it would let the destination
		// user — who can see Server.UUID — authenticate as the agent
		// during the Pending window. Reject the user-global secret until
		// the transfer settles; the HandshakeSecret path in check() is
		// the only valid promotion route.
		if singleton.ServerTransferShared != nil {
			if _, ok := singleton.ServerTransferShared.LookupPending(cid); ok {
				return 0, false, fmt.Errorf("destination user's global AgentSecret cannot authenticate during a pending transfer; agent must rotate to per-transfer HandshakeSecret")
			}
		}
		return cid, true, nil
	}
	// server.UserID != userId — normally an impersonation attempt. Allow it
	// only when a ServerTransfer for this server is Pending AND the secret in
	// hand is the previous owner's (FromUserID), OR when a recently terminated
	// transfer left a revert-delivery for FromUserID and the agent is still
	// presenting its pre-transfer global secret.
	//
	// SECURITY: we deliberately do NOT accept the destination user's global
	// AgentSecret on the LookupRevertDelivery path. PushIfOnline only ever
	// delivers per-transfer HandshakeSecret / RevertHandshakeSecret to the
	// agent — the ToUserID global secret never travels over the wire — so a
	// reconnect under that credential is not proof of agent rotation; it can
	// only come from the destination user themselves, who can see Server.UUID
	// once Register flips Server.UserID. Admitting it would let that user
	// impersonate the agent during the rollback window, trigger
	// pushRevertIfOnline to leak RevertHandshakeSecret, and then be promoted
	// into verifiedHandshakes via MarkRevertDelivered. The legitimate recovery
	// paths are: FromUserID global secret (handled below), forward
	// HandshakeSecret and RevertHandshakeSecret (handled by the
	// terminalSecretRecovery / verifiedHandshakes lookups in check()).
	if singleton.ServerTransferShared != nil {
		if t, ok := singleton.ServerTransferShared.LookupRevertDelivery(cid); ok && t.FromUserID == userId {
			return cid, true, nil
		}
		if t, ok := singleton.ServerTransferShared.LookupPending(cid); ok && t.FromUserID == userId {
			return cid, true, nil
		}
	}
	return 0, false, fmt.Errorf("client UUID does not belong to the agent secret owner")
}
