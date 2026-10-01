package model

import (
	"errors"
	"log"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"
	"gorm.io/gorm"

	pb "github.com/nezhahq/nezha/proto"
)

var runtimeHolderInitMu sync.Mutex
var nextServerReportSequence atomic.Uint64

type Server struct {
	Common

	Name                   string `json:"name"`
	UUID                   string `json:"uuid,omitempty" gorm:"unique"`
	Note                   string `json:"note,omitempty"`           // 管理员可见备注
	PublicNote             string `json:"public_note,omitempty"`    // 公开备注
	DisplayIndex           int    `json:"display_index"`            // 展示排序，越大越靠前
	HideForGuest           bool   `json:"hide_for_guest,omitempty"` // 对游客隐藏
	EnableDDNS             bool   `json:"enable_ddns,omitempty"`    // 启用DDNS
	DDNSProfilesRaw        string `gorm:"default:'[]';column:ddns_profiles_raw" json:"-"`
	OverrideDDNSDomainsRaw string `gorm:"default:'{}';column:override_ddns_domains_raw" json:"-"`

	DDNSProfiles        []uint64            `gorm:"-" json:"ddns_profiles,omitempty" validate:"optional"` // DDNS配置
	OverrideDDNSDomains map[uint64][]string `gorm:"-" json:"override_ddns_domains,omitempty" validate:"optional"`

	Host       *Host      `gorm:"-" json:"host,omitempty"`
	State      *HostState `gorm:"-" json:"state,omitempty"`
	GeoIP      *GeoIP     `gorm:"-" json:"geoip,omitempty"`
	LastActive time.Time  `gorm:"-" json:"last_active,omitempty"`

	// taskStream MUST be accessed only via SetTaskStream / GetTaskStream. Direct
	// field access from outside this file races with the gRPC RequestTask
	// handler that reassigns the stream on every reconnect — a torn read of the
	// two-word interface header would panic on a subsequent .Send call. The
	// atomic.Pointer + holder struct lets us swap the stream lock-free while
	// every reader observes a single, consistent value. The holder also carries
	// the send mutex so CopyFromRunningServer can share it across the old/new
	// *Server objects that briefly co-exist during edit/transfer rotations —
	// otherwise two *Server pointers would hold the same gRPC stream behind
	// two independent mutexes, defeating the "one SendMsg goroutine per stream"
	// invariant grpc-go requires.
	taskStream  atomic.Pointer[taskStreamHolder]
	runtime     atomic.Pointer[serverRuntimeHolder]
	ConfigCache chan any `gorm:"-" json:"-"`

	PrevTransferInSnapshot  uint64 `gorm:"-" json:"-"` // 上次数据点时的入站使用量
	PrevTransferOutSnapshot uint64 `gorm:"-" json:"-"` // 上次数据点时的出站使用量
}

// taskStreamHolder wraps the interface so atomic.Pointer (which requires a
// concrete pointed-to type) can publish it atomically. The previous bare
// field `TaskStream pb.NezhaService_RequestTaskServer` was a plain interface
// value: two words on the heap (type ptr + data ptr). Concurrent assignment
// produced torn reads detectable by `go test -race` and crashable in production.
//
// sendMu lives on the holder (not on *Server) so it is bound to the stream
// itself: CopyFromRunningServer shares the same holder pointer with the new
// *Server, and SendTask locks via the holder, guaranteeing serialized SendMsg
// even when old/new *Server objects briefly co-exist during edit/transfer.
type taskStreamHolder struct {
	s      pb.NezhaService_RequestTaskServer
	sendMu sync.Mutex
}

type serverRuntimeHolder struct {
	mu             sync.Mutex
	canonical      *Server
	stream         pb.NezhaService_ReportSystemStateServer
	generation     uint64
	state          *HostState
	host           *Host
	lastActive     time.Time
	reportSequence uint64
	prevIn         uint64
	prevOut        uint64
}

type StateStreamLease struct {
	holder     *serverRuntimeHolder
	generation uint64
}

func (lease StateStreamLease) Generation() uint64 {
	return lease.generation
}

type RuntimeHandle struct {
	holder *serverRuntimeHolder
}

type HostReportResult struct {
	ServerID uint64
	UUID     string
	Applied  bool
	Initial  bool
	Equal    bool
	Stale    bool
	Restart  bool
	Transfer Transfer
}

func (s *Server) RuntimeHandle() RuntimeHandle {
	runtimeHolderInitMu.Lock()
	holder := s.runtime.Load()
	if holder == nil {
		holder = &serverRuntimeHolder{canonical: s, state: cloneHostState(s.State), host: cloneHost(s.Host), lastActive: s.LastActive, prevIn: s.PrevTransferInSnapshot, prevOut: s.PrevTransferOutSnapshot}
		s.runtime.Store(holder)
	}
	runtimeHolderInitMu.Unlock()
	return RuntimeHandle{holder: holder}
}

func (handle RuntimeHandle) ApplyHostReport(host *Host, createdAt time.Time, persist func(Transfer) error) (HostReportResult, error) {
	if handle.holder == nil || host == nil {
		return HostReportResult{}, errors.New("invalid runtime handle")
	}
	holder := handle.holder
	holder.mu.Lock()
	defer holder.mu.Unlock()
	canonical := holder.canonical
	if canonical == nil {
		return HostReportResult{}, errors.New("runtime handle has no canonical server")
	}
	result := HostReportResult{ServerID: canonical.ID, UUID: canonical.UUID}
	if holder.host == nil {
		holder.host = cloneHost(host)
		canonical.Host = cloneHost(host)
		result.Applied = true
		result.Initial = true
		return result, nil
	}
	if host.BootTime < holder.host.BootTime {
		result.Stale = true
		return result, nil
	}
	if host.BootTime == holder.host.BootTime {
		holder.host = cloneHost(host)
		canonical.Host = cloneHost(host)
		result.Applied = true
		result.Equal = true
		return result, nil
	}
	result.Restart = true
	if holder.state != nil {
		result.Transfer = Transfer{Common: Common{CreatedAt: createdAt}, ServerID: canonical.ID, In: holder.state.NetInTransfer - min(holder.state.NetInTransfer, holder.prevIn), Out: holder.state.NetOutTransfer - min(holder.state.NetOutTransfer, holder.prevOut)}
	}
	if persist != nil {
		if err := persist(result.Transfer); err != nil {
			return HostReportResult{}, err
		}
	}
	holder.host = cloneHost(host)
	holder.state = &HostState{}
	holder.lastActive = time.Time{}
	holder.prevIn, holder.prevOut = 0, 0
	canonical.Host = cloneHost(host)
	canonical.State = &HostState{}
	canonical.LastActive = time.Time{}
	canonical.PrevTransferInSnapshot = 0
	canonical.PrevTransferOutSnapshot = 0
	result.Applied = true
	return result, nil
}

func (lease StateStreamLease) UpdateState(state *HostState, lastActive time.Time) bool {
	return lease.UpdateStateWithSideEffect(state, lastActive, nil)
}

func (lease StateStreamLease) UpdateStateWithSideEffect(state *HostState, lastActive time.Time, sideEffect func() error) bool {
	return lease.updateState(nil, state, lastActive, sideEffect)
}

func (lease StateStreamLease) updateState(receiver *Server, state *HostState, lastActive time.Time, sideEffect func() error) bool {
	if lease.holder == nil {
		return false
	}
	lease.holder.mu.Lock()
	defer lease.holder.mu.Unlock()
	if lease.holder.generation != lease.generation || lease.holder.stream == nil || lease.holder.canonical == nil || (receiver != nil && lease.holder.canonical != receiver) {
		return false
	}
	canonical := lease.holder.canonical
	canonical.State = cloneHostState(state)
	canonical.LastActive = lastActive
	lease.holder.state = cloneHostState(state)
	lease.holder.lastActive = lastActive
	if lease.holder.prevIn == 0 || lease.holder.prevOut == 0 {
		lease.holder.prevIn = state.NetInTransfer
		lease.holder.prevOut = state.NetOutTransfer
	}
	canonical.PrevTransferInSnapshot = lease.holder.prevIn
	canonical.PrevTransferOutSnapshot = lease.holder.prevOut
	if sideEffect != nil {
		if err := sideEffect(); err != nil {
			return false
		}
	}
	lease.holder.reportSequence = nextServerReportSequence.Add(1)
	return true
}

func (lease StateStreamLease) Clear() bool {
	return lease.clear(nil)
}

func (lease StateStreamLease) clear(receiver *Server) bool {
	if lease.holder == nil {
		return false
	}
	lease.holder.mu.Lock()
	defer lease.holder.mu.Unlock()
	if lease.holder.generation != lease.generation || lease.holder.stream == nil || lease.holder.canonical == nil || (receiver != nil && lease.holder.canonical != receiver) {
		return false
	}
	lease.holder.stream = nil
	lease.holder.lastActive = time.Time{}
	lease.holder.canonical.LastActive = time.Time{}
	return true
}

// SetTaskStream publishes the agent's RequestTask stream so other goroutines
// can deliver tasks to the agent. Pass nil to detach (e.g. on disconnect).
func (s *Server) SetTaskStream(stream pb.NezhaService_RequestTaskServer) {
	if stream == nil {
		s.taskStream.Store(nil)
		return
	}
	s.taskStream.Store(&taskStreamHolder{s: stream})
}

// adoptTaskStreamHolder publishes an existing holder verbatim. Used by
// CopyFromRunningServer so the new *Server shares the send mutex (and the
// underlying stream identity) with the old *Server.
func (s *Server) adoptTaskStreamHolder(h *taskStreamHolder) {
	s.taskStream.Store(h)
}

// ClearTaskStreamIfCurrent detaches stream only if it is still the published
// RequestTask stream. Disconnect cleanup uses this guard so an old stream
// returning after a reconnect cannot erase the newer live stream.
func (s *Server) ClearTaskStreamIfCurrent(stream pb.NezhaService_RequestTaskServer) bool {
	if stream == nil {
		return false
	}
	for {
		h := s.taskStream.Load()
		if h == nil || h.s != stream {
			return false
		}
		if s.taskStream.CompareAndSwap(h, nil) {
			return true
		}
	}
}

// GetTaskStream returns the currently-published stream, or nil if the agent
// is offline. Callers MUST capture the return into a local variable before
// using it — re-reading via GetTaskStream() across a Send call reopens the
// race we're trying to close.
func (s *Server) GetTaskStream() pb.NezhaService_RequestTaskServer {
	h := s.taskStream.Load()
	if h == nil {
		return nil
	}
	return h.s
}

// SendTask dispatches a task on the agent's RequestTask stream under the
// holder's sendMu so concurrent dispatchers (cron, server-transfer
// ApplyConfig, MCP CallAgent, MCP fs.transfer, force-update, report-config)
// cannot violate grpc-go's "one SendMsg goroutine per stream" rule. Returns
// ErrTaskStreamOffline if the agent has not published a stream yet; callers
// that need to distinguish offline from send failure should branch on that.
//
// The mutex is keyed by holder (= by stream) rather than by *Server so that
// edit/transfer rotations replacing *Server in the singleton map still share
// a single lock across the old and new objects pointing at the same stream.
func (s *Server) SendTask(task *pb.Task) error {
	h := s.taskStream.Load()
	if h == nil {
		return ErrTaskStreamOffline
	}
	h.sendMu.Lock()
	defer h.sendMu.Unlock()
	return h.s.Send(task)
}

// AttachStateStream returns the ownership generation used to serialize state
// writes with reconnect and disconnect cleanup.
func (s *Server) AttachStateStream(stream pb.NezhaService_ReportSystemStateServer) StateStreamLease {
	if stream == nil {
		return StateStreamLease{}
	}
	runtimeHolderInitMu.Lock()
	defer runtimeHolderInitMu.Unlock()
	holder := s.runtime.Load()
	if holder == nil {
		candidate := &serverRuntimeHolder{canonical: s, state: cloneHostState(s.State), host: cloneHost(s.Host), lastActive: s.LastActive, prevIn: s.PrevTransferInSnapshot, prevOut: s.PrevTransferOutSnapshot}
		if s.runtime.CompareAndSwap(nil, candidate) {
			holder = candidate
		} else {
			holder = s.runtime.Load()
		}
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	holder.generation++
	holder.stream = stream
	return StateStreamLease{holder: holder, generation: holder.generation}
}

func (s *Server) UpdateStateIfCurrent(lease StateStreamLease, state *HostState, lastActive time.Time) bool {
	return s.UpdateStateIfCurrentWithSideEffect(lease, state, lastActive, nil)
}

func (s *Server) UpdateStateIfCurrentWithSideEffect(lease StateStreamLease, state *HostState, lastActive time.Time, sideEffect func() error) bool {
	return lease.updateState(s, state, lastActive, sideEffect)
}

func (s *Server) ClearStateStreamIfCurrent(lease StateStreamLease) bool {
	return lease.clear(s)
}

// RuntimeSnapshot is a deep copy of the mutable runtime state.
type RuntimeSnapshot struct {
	State                   *HostState
	Host                    *Host
	LastActive              time.Time
	ReportSequence          uint64
	PrevTransferInSnapshot  uint64
	PrevTransferOutSnapshot uint64
}

func (s *Server) RuntimeSnapshot() RuntimeSnapshot {
	runtimeHolderInitMu.Lock()
	holder := s.runtime.Load()
	if holder == nil {
		candidate := &serverRuntimeHolder{canonical: s, state: cloneHostState(s.State), lastActive: s.LastActive, prevIn: s.PrevTransferInSnapshot, prevOut: s.PrevTransferOutSnapshot}
		if s.runtime.CompareAndSwap(nil, candidate) {
			holder = candidate
		} else {
			holder = s.runtime.Load()
		}
	}
	runtimeHolderInitMu.Unlock()
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if holder.canonical == s {
		if holder.state == nil {
			holder.state = cloneHostState(s.State)
		}
		if holder.host == nil {
			holder.host = cloneHost(s.Host)
		}
	}
	return RuntimeSnapshot{State: cloneHostState(holder.state), Host: cloneHost(holder.host), LastActive: holder.lastActive, ReportSequence: holder.reportSequence, PrevTransferInSnapshot: holder.prevIn, PrevTransferOutSnapshot: holder.prevOut}
}

func (s *Server) SetTransferSnapshots(inbound, outbound uint64) bool {
	runtimeHolderInitMu.Lock()
	holder := s.runtime.Load()
	if holder == nil {
		holder = &serverRuntimeHolder{canonical: s, state: cloneHostState(s.State), lastActive: s.LastActive}
		s.runtime.Store(holder)
	}
	runtimeHolderInitMu.Unlock()
	holder.mu.Lock()
	if holder.canonical != s {
		holder.mu.Unlock()
		return false
	}
	holder.prevIn = inbound
	holder.prevOut = outbound
	if holder.canonical != nil {
		holder.canonical.PrevTransferInSnapshot = inbound
		holder.canonical.PrevTransferOutSnapshot = outbound
	}
	holder.mu.Unlock()
	return true
}

func (s *Server) TransferSnapshotDelta() (inbound, outbound, snapshotIn, snapshotOut uint64) {
	snapshot := s.RuntimeSnapshot()
	if snapshot.State == nil {
		return 0, 0, snapshot.PrevTransferInSnapshot, snapshot.PrevTransferOutSnapshot
	}
	return snapshot.State.NetInTransfer, snapshot.State.NetOutTransfer, snapshot.PrevTransferInSnapshot, snapshot.PrevTransferOutSnapshot
}

func (s *Server) TransferDeltaAndAdvance() (inbound, outbound uint64, deltaIn, deltaOut uint64) {
	runtimeHolderInitMu.Lock()
	holder := s.runtime.Load()
	if holder == nil {
		holder = &serverRuntimeHolder{canonical: s, state: cloneHostState(s.State), host: cloneHost(s.Host), lastActive: s.LastActive, prevIn: s.PrevTransferInSnapshot, prevOut: s.PrevTransferOutSnapshot}
		s.runtime.Store(holder)
	}
	runtimeHolderInitMu.Unlock()
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if holder.canonical != s || holder.state == nil {
		return 0, 0, 0, 0
	}
	inbound, outbound = holder.state.NetInTransfer, holder.state.NetOutTransfer
	deltaIn = inbound - min(inbound, holder.prevIn)
	deltaOut = outbound - min(outbound, holder.prevOut)
	holder.prevIn, holder.prevOut = inbound, outbound
	if holder.canonical != nil {
		holder.canonical.PrevTransferInSnapshot = inbound
		holder.canonical.PrevTransferOutSnapshot = outbound
	}
	return
}

func cloneHostState(state *HostState) *HostState {
	if state == nil {
		return nil
	}
	clone := *state
	clone.GPU = slices.Clone(state.GPU)
	clone.Temperatures = slices.Clone(state.Temperatures)
	return &clone
}

func cloneHost(host *Host) *Host {
	if host == nil {
		return nil
	}
	clone := *host
	clone.CPU = slices.Clone(host.CPU)
	clone.GPU = slices.Clone(host.GPU)
	return &clone
}

func (s *Server) SetHost(host *Host) bool {
	runtimeHolderInitMu.Lock()
	holder := s.runtime.Load()
	if holder == nil {
		holder = &serverRuntimeHolder{canonical: s, state: cloneHostState(s.State), lastActive: s.LastActive}
		s.runtime.Store(holder)
	}
	runtimeHolderInitMu.Unlock()
	holder.mu.Lock()
	if holder.canonical != s {
		holder.mu.Unlock()
		return false
	}
	holder.host = cloneHost(host)
	if holder.canonical != nil {
		holder.canonical.Host = cloneHost(host)
	}
	holder.mu.Unlock()
	return true
}

// ErrTaskStreamOffline is returned by SendTask when the agent has no
// published RequestTask stream. Defined here (rather than in service/rpc)
// so model-layer callers can branch on it without an import cycle.
var ErrTaskStreamOffline = errors.New("agent task stream offline")

func InitServer(s *Server) {
	s.Host = &Host{}
	s.State = &HostState{}
	s.GeoIP = &GeoIP{}
	s.ConfigCache = make(chan any, 1)
	s.runtime.Store(&serverRuntimeHolder{canonical: s, state: cloneHostState(s.State), host: cloneHost(s.Host)})
}

func (s *Server) CopyFromRunningServer(old *Server) {
	runtimeHolderInitMu.Lock()
	defer runtimeHolderInitMu.Unlock()
	s.GeoIP = old.GeoIP
	// Adopt the holder pointer verbatim so the new *Server shares the send
	// mutex AND the stream identity with the old *Server; constructing a fresh
	// holder via SetTaskStream(GetTaskStream()) would give the new object its
	// own mutex, letting two *Server pointers race SendMsg on the same stream
	// during the edit/transfer rotation window.
	s.adoptTaskStreamHolder(old.taskStream.Load())
	holder := old.runtime.Load()
	if holder == nil {
		holder = &serverRuntimeHolder{canonical: old, state: cloneHostState(old.State), host: cloneHost(old.Host), lastActive: old.LastActive, prevIn: old.PrevTransferInSnapshot, prevOut: old.PrevTransferOutSnapshot}
		old.runtime.CompareAndSwap(nil, holder)
		holder = old.runtime.Load()
	}
	holder.mu.Lock()
	holder.canonical = s
	s.runtime.Store(holder)
	s.State = cloneHostState(holder.state)
	s.Host = cloneHost(holder.host)
	s.LastActive = holder.lastActive
	s.PrevTransferInSnapshot = holder.prevIn
	s.PrevTransferOutSnapshot = holder.prevOut
	holder.mu.Unlock()
	s.ConfigCache = old.ConfigCache
}

func (s *Server) AfterFind(tx *gorm.DB) error {
	if s.DDNSProfilesRaw != "" {
		if err := json.Unmarshal([]byte(s.DDNSProfilesRaw), &s.DDNSProfiles); err != nil {
			log.Println("NEZHA>> Server.AfterFind:", err)
			return nil
		}
	}
	if s.OverrideDDNSDomainsRaw != "" {
		if err := json.Unmarshal([]byte(s.OverrideDDNSDomainsRaw), &s.OverrideDDNSDomains); err != nil {
			log.Println("NEZHA>> Server.AfterFind:", err)
			return nil
		}
	}
	return nil
}

// ServerOwnerInfo carries the user-facing identity for Server.UserID. It is
// returned by the lookup function installed by the singleton layer; model
// must not import singleton (cycle), so the dependency flows through a
// package-level function variable instead.
type ServerOwnerInfo struct {
	ID       uint64 `json:"id"`
	Username string `json:"username,omitempty"`
}

// ServerOwnerLookup is installed by singleton at startup to resolve a
// Server.UserID into a display-ready owner record. Returns ok=false when
// the uid does not map to a known user; the caller renders that as an
// "unknown user" placeholder so deleted-user rows stay debuggable. Left nil
// in tests / headless contexts so the JSON simply omits the owner field.
var ServerOwnerLookup func(uid uint64) (ServerOwnerInfo, bool)

// OwnerServerIDsLookup is installed by singleton at startup to enumerate the
// IDs of every in-memory Server whose UserID == ownerUID. It exists so that
// Cron.HasPermission / Service.HasPermission can faithfully replay the
// dispatch-side "CoverAll deny-list must cover every PAT-whitelisted-out
// owner server" rule without depending on controller helpers (model must
// not import service/singleton — cycle).
//
// Left nil in tests / headless contexts; callers MUST treat a nil hook as
// "unknown owner topology" and fall back to a conservative decision (the
// existing model.Cron / model.Service code rejects non-trivial CoverAll
// configs for limited PATs when the hook is nil, matching the historical
// behaviour for empty deny-lists).
var OwnerServerIDsLookup func(ownerUID uint64) []uint64

// OwnerIsAdminLookup reports whether ownerUID is an admin user. When the
// owner is admin the runtime dispatch path (CronTrigger, DispatchTask) gates
// on userIsAdmin(cr.UserID) / userIsAdmin(svc.UserID) and fans out across
// EVERY in-memory server — not just the owner's. DenyListSafeForLimitedPAT
// must mirror that fan-out widening or a limited PAT can pass safety check
// with a deny-list that covers only the admin's own servers while the
// runtime still ships the task to foreign-owned servers.
//
// Left nil in tests / headless contexts; callers fall back to
// "owner-set only" which matches the pre-C1 behaviour.
var OwnerIsAdminLookup func(ownerUID uint64) bool

// AllServerIDsLookup returns every in-memory server ID, regardless of
// owner. It is the system-wide fan-out set the runtime uses for
// admin-owned CoverAll cron/service dispatch and is the only correct
// containment set for a server-limited PAT operating on an admin-owned
// resource. Left nil in tests / headless contexts.
var AllServerIDsLookup func() []uint64

type serverJSON Server

type serverWithOwner struct {
	*serverJSON
	Owner *ServerOwnerInfo `json:"owner,omitempty"`
}

// MarshalJSON projects Server.UserID into a structured owner field on the
// wire. Server.UserID itself stays `json:"-"` (set on Common) so callers
// that do not need owner info pay nothing and members do not accidentally
// receive raw uid integers. The lookup function is consulted only when
// installed; if absent we still emit a minimal {id} record so clients can
// at least distinguish ownership, except for uid=0 which is the legacy
// global-secret pseudo-owner and is best surfaced as such by the caller's
// translation table on the frontend.
func (s *Server) MarshalJSON() ([]byte, error) {
	runtime := s.RuntimeSnapshot()
	copy := s.RuntimeCopy(runtime)
	owner := &ServerOwnerInfo{ID: s.GetUserID()}
	if ServerOwnerLookup != nil {
		if info, ok := ServerOwnerLookup(owner.ID); ok {
			owner.Username = info.Username
		}
	}
	return json.Marshal(serverWithOwner{
		serverJSON: (*serverJSON)(copy),
		Owner:      owner,
	})
}

func (s *Server) RuntimeCopy(runtime RuntimeSnapshot) *Server {
	return &Server{
		Common: Common{
			ID:        s.ID,
			CreatedAt: s.CreatedAt,
			UpdatedAt: s.UpdatedAt,
			UserID:    s.GetUserID(),
		},
		Name:                    s.Name,
		UUID:                    s.UUID,
		Note:                    s.Note,
		PublicNote:              s.PublicNote,
		DisplayIndex:            s.DisplayIndex,
		HideForGuest:            s.HideForGuest,
		EnableDDNS:              s.EnableDDNS,
		DDNSProfilesRaw:         s.DDNSProfilesRaw,
		OverrideDDNSDomainsRaw:  s.OverrideDDNSDomainsRaw,
		DDNSProfiles:            slices.Clone(s.DDNSProfiles),
		OverrideDDNSDomains:     s.OverrideDDNSDomains,
		Host:                    runtime.Host,
		State:                   runtime.State,
		GeoIP:                   s.GeoIP,
		LastActive:              runtime.LastActive,
		ConfigCache:             s.ConfigCache,
		PrevTransferInSnapshot:  runtime.PrevTransferInSnapshot,
		PrevTransferOutSnapshot: runtime.PrevTransferOutSnapshot,
	}
}

func (s *Server) HasPermission(ctx *gin.Context) bool {
	if !s.Common.HasPermission(ctx) {
		return false
	}
	v, ok := ctx.Get(CtxKeyAPIToken)
	if !ok {
		return true
	}
	tok, ok := v.(APITokenAccessor)
	if !ok || tok == nil {
		return true
	}
	return tok.CanAccessServer(s.GetID())
}

// APITokenWhitelistView is the optional shape an APITokenAccessor can
// implement so DenyListSafeForLimitedPAT can tell unscoped PATs (no
// whitelist → not limited) apart from server-limited ones. Accessors that
// do NOT expose ServerIDs() are treated as potentially limited; the safe
// dispatch path then requires denyList to cover every owner-visible server
// outside what the PAT can reach.
type APITokenWhitelistView interface {
	ServerIDs() []uint64
}

// DenyListSafeForLimitedPAT reports whether a CoverAll/SkipServers deny-list
// keeps a server-limited PAT inside its server_ids whitelist. The runtime
// dispatch path (CronTrigger, DispatchTask) iterates every owner-visible
// server minus denyList; for the PAT to stay contained, every owner server
// outside its whitelist must already appear in denyList. JWT requests and
// PATs with no whitelist are unaffected. Nil OwnerServerIDsLookup forces
// the conservative "reject" branch instead of silently allowing a config
// the runtime would dispatch outside the whitelist.
func DenyListSafeForLimitedPAT(tok APITokenAccessor, ownerUID uint64, denyServers []uint64) bool {
	if tok == nil {
		return true
	}
	if wl, ok := tok.(APITokenWhitelistView); ok && len(wl.ServerIDs()) == 0 {
		return true
	}
	fanout := ownerEffectiveFanoutServerIDs(ownerUID)
	if fanout == nil {
		return false
	}
	denySet := make(map[uint64]struct{}, len(denyServers))
	for _, id := range denyServers {
		denySet[id] = struct{}{}
	}
	for _, id := range fanout {
		if tok.CanAccessServer(id) {
			continue
		}
		if _, denied := denySet[id]; !denied {
			return false
		}
	}
	return true
}

// ownerEffectiveFanoutServerIDs returns the server set the runtime dispatch
// will actually fan out to for a resource owned by ownerUID. Admin owners
// short-circuit cronCanSendToServer / canSendServiceTask via userIsAdmin,
// so the safe containment set is the WHOLE system, not just the admin's
// own servers. Member owners stay bounded to their own server set.
//
// Returns nil to signal "topology unknown" — callers (DenyListSafeForLimitedPAT)
// fall back to fail-closed in that case, matching the historical conservative
// branch when OwnerServerIDsLookup was nil.
func ownerEffectiveFanoutServerIDs(ownerUID uint64) []uint64 {
	if OwnerIsAdminLookup != nil && OwnerIsAdminLookup(ownerUID) {
		if AllServerIDsLookup == nil {
			return nil
		}
		return AllServerIDsLookup()
	}
	if OwnerServerIDsLookup == nil {
		return nil
	}
	return OwnerServerIDsLookup(ownerUID)
}

func (s *Server) SplitList(x []*Server) ([]*Server, []*Server) {
	pri := func(s *Server) bool {
		return s.DisplayIndex == 0
	}

	i := slices.IndexFunc(x, pri)
	if i == -1 {
		return nil, x
	}

	return x[:i], x[i:]
}
