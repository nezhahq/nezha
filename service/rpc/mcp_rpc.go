package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

// MCP 的"调用-响应"模式复用了 RequestTask 双向流：
//   - dashboard 发 Task（带新分配的 taskID + JSON params）
//   - agent 执行后回 TaskResult（同 taskID + JSON result）
//   - RequestTask 接收循环把这种 TaskType 识别后路由到 inflight 等待方
//
// 不污染 model.Server 字段：用本包内的全局 inflight 表按 taskID 关联，
// 跨 server 共享单一命名空间。

var (
	mcpTaskIDCounter atomic.Uint64
	mcpInflight      sync.Map // key: uint64 (taskID), value: chan *pb.TaskResult
)

// ErrMCPDisabled 是 CallAgent 在 MCP kill switch 被触发时返回的哨兵错误。
// 与 ErrAgentTimeout / ErrAgentOffline 平级，便于 controller 把它映射到
// MCPOutcomeForbidden 之类的审计 code 而不是误报 agent 故障。
var ErrMCPDisabled = errors.New("MCP is disabled by the dashboard administrator")

// mcpKillSwitchObserved is a process-level hook the dashboard wires to
// singleton.Conf.EnableMCP. CallAgent consults it before any side-effects so
// the entry-check / cancel-sweep / registration race cannot leak a fresh
// call past EnableMCP=false. Defaults to "disarmed" so tests and headless
// builds are unaffected.
//
// Stored behind atomic.Pointer because SetMCPKillSwitchObserver (startup +
// tests) and CallAgent (any RPC goroutine) touch it concurrently; a plain
// func variable is a data race under -race.
var mcpKillSwitchObserved atomic.Pointer[func() bool]

// disarmedKillSwitch is the default probe: never trips the kill switch.
var disarmedKillSwitch = func() bool { return false }

// testKillSwitchAfterUpfrontCheck, when non-nil, runs inside CallAgent between
// the upfront kill-switch check and the inflight registration. Production
// leaves it nil; tests use it to drive the registration-after-sweep race
// deterministically.
var testKillSwitchAfterUpfrontCheck atomic.Pointer[func()]

var (
	testMCPResultBeforeCancellationCheck atomic.Pointer[func()]
	testMCPResultAfterCancellationCheck  atomic.Pointer[func()]
)

// SetMCPKillSwitchObserver installs the kill-switch probe the dashboard
// owns. Idempotent; the dashboard wires it at startup. Passing nil
// restores the default disarmed hook (used by tests to undo overrides).
func SetMCPKillSwitchObserver(fn func() bool) {
	if fn == nil {
		mcpKillSwitchObserved.Store(&disarmedKillSwitch)
		return
	}
	mcpKillSwitchObserved.Store(&fn)
}

// mcpKillSwitchObserver returns the currently installed probe, never nil.
func mcpKillSwitchObserver() func() bool {
	if p := mcpKillSwitchObserved.Load(); p != nil {
		return *p
	}
	return disarmedKillSwitch
}

// allocateMCPTaskID 分配下一个 MCP 用的 task ID。
// 取 1<<32 起步以与可能存在的 cron/transfer 等已有 ID 空间错开（cron.id 由
// DB 自增，常量级，不会触及 1<<32）。
func allocateMCPTaskID() uint64 {
	const base uint64 = 1 << 32
	v := mcpTaskIDCounter.Add(1)
	return base + v
}

// CallAgent 给 serverID 对应的 agent 发一条 MCP-RPC 风格的 Task，并阻塞等待 TaskResult 回包。
//
// taskType 必须是 model.IsMCPRPCResult 返回 true 的类型；params 会被 JSON 编码进 Task.Data。
// 超时由调用方控制；触发超时后从 inflight 表移除等待 slot（晚到的回包会被丢弃）。
//
// 错误语义：
//   - server 未在线 / 未连接 task stream → ErrAgentOffline
//   - 超时 → ctx.Err 或 ErrAgentTimeout
//   - agent 回包 successful=false → 把 result.Data 当错误字符串返回
//   - CancelAllMCPInflight 期间被中断 → ErrMCPDisabled
//   - 任何 send 失败、序列化失败 → 原始 error
//
// 返回的 raw JSON 是 agent 端 TaskResult.Data 的原文。
func CallAgent(ctx context.Context, serverID uint64, taskType uint64, params any, timeout time.Duration) (json.RawMessage, error) {
	releaseControl, controlErr := singleton.BeginServerControlFromContext(ctx, serverID)
	if controlErr != nil {
		return nil, controlErr
	}
	defer releaseControl()
	if !model.IsMCPRPCResult(taskType) {
		return nil, errors.New("CallAgent: task type is not registered as MCP RPC")
	}

	killSwitch := mcpKillSwitchObserver()
	if killSwitch() {
		return nil, ErrMCPDisabled
	}

	server, _ := singleton.ServerShared.Get(serverID)
	if server == nil {
		return nil, ErrAgentOffline
	}
	if server.GetTaskStream() == nil {
		return nil, ErrAgentOffline
	}

	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	taskID := allocateMCPTaskID()
	resultCh := make(chan *pb.TaskResult, 1)
	cancelCh := make(chan struct{})
	entry := &mcpInflightEntry{
		serverID:  serverID,
		result:    resultCh,
		cancel:    cancelCh,
		cancelled: new(atomic.Bool),
	}

	if hook := testKillSwitchAfterUpfrontCheck.Load(); hook != nil {
		(*hook)()
	}

	mcpInflight.Store(taskID, entry)
	defer mcpInflight.Delete(taskID)

	// Close the registration-after-sweep window: a kill switch that fired
	// between the upfront check and this Store is invisible to
	// CancelAllMCPInflight (our entry was not in the map yet). Because the
	// operator sets EnableMCP=false BEFORE running the sweep, re-reading the
	// observer here after Store guarantees we either see it disabled, or the
	// sweep saw our now-registered entry and flipped entry.cancelled.
	if killSwitch() || entry.cancelled.Load() {
		return nil, ErrMCPDisabled
	}

	if err := server.SendTask(&pb.Task{
		Id:   taskID,
		Type: taskType,
		Data: string(body),
	}); err != nil {
		if errors.Is(err, model.ErrTaskStreamOffline) {
			return nil, ErrAgentOffline
		}
		return nil, err
	}
	notifyMCPTaskDispatched(serverID, taskID, taskType)

	waitCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	select {
	case res := <-resultCh:
		if hook := testMCPResultBeforeCancellationCheck.Load(); hook != nil {
			(*hook)()
		}
		// Cancel must beat a late agent reply: Go select picks a random
		// ready case, so if CancelAllMCPInflight closed cancelCh after the
		// agent already filled resultCh we could still surface success.
		// Re-check the cancel flag and prefer ErrMCPDisabled, matching the
		// contract documented above ("CancelAllMCPInflight 期间被中断 →
		// ErrMCPDisabled") and what TestUpdateConfig_DisablingMCPInvokesKillSwitch
		// expects.
		if !entry.claimResult() {
			return nil, ErrMCPDisabled
		}
		notifyMCPTaskResultAccepted(entry.serverID, res.GetId(), res.GetType())
		if hook := testMCPResultAfterCancellationCheck.Load(); hook != nil {
			(*hook)()
		}
		if res == nil {
			return nil, errors.New("agent returned nil result")
		}
		if !res.GetSuccessful() {
			if res.GetData() != "" {
				return nil, errors.New(res.GetData())
			}
			return nil, errors.New("agent returned unsuccessful result")
		}
		return json.RawMessage(res.GetData()), nil
	case <-cancelCh:
		return nil, ErrMCPDisabled
	case <-waitCtx.Done():
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrAgentTimeout
		}
		return nil, waitCtx.Err()
	}
}

// mcpInflightEntry binds an in-flight MCP call to its target serverID and
// pairs the result channel with a per-call cancel channel so the kill switch
// can break out of CallAgent without leaving the result channel dangling for
// the next late agent reply. The serverID is the authoritative reporter
// identity check at delivery time — without it deliverMCPResult would route
// purely by attacker-controlled TaskResult.Id (same bug class as commit
// 02129f1 in the cron path).
//
// cancelled flips to true when CancelAllMCPInflight wins the entry lock before
// the result is claimed. Every code path that could complete the call — the
// CallAgent select on resultCh, deliverMCPResult, deliverMCPResultFromReporter
// — MUST consult it before treating an agent reply as authoritative.
type mcpInflightEntry struct {
	serverID  uint64
	result    chan *pb.TaskResult
	cancel    chan struct{}
	cancelled *atomic.Bool
	mu        sync.Mutex
	claimed   bool
	closeOnce sync.Once
}

func (e *mcpInflightEntry) claimResult() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancelled.Load() {
		return false
	}
	e.claimed = true
	return true
}

func (e *mcpInflightEntry) cancelCall() {
	e.mu.Lock()
	if !e.claimed {
		e.cancelled.Store(true)
	}
	e.mu.Unlock()
	e.closeCancel()
}

// closeCancel closes the entry's cancel channel exactly once. Concurrent
// CancelAllMCPInflight sweeps (two admin PATCH /setting requests both
// disabling MCP) would otherwise race a non-atomic check-then-close and
// panic on the second close.
func (e *mcpInflightEntry) closeCancel() {
	e.closeOnce.Do(func() { close(e.cancel) })
}

// CancelAllMCPInflight closes every in-flight CallAgent so they return
// ErrMCPDisabled immediately. Used by the EnableMCP=false transition: by
// itself the inflight table holds the dashboard goroutine hostage until
// the agent replies (or the per-call timeout fires, up to ~305s for
// server.exec). Returns the number of calls cancelled for audit.
//
// Implementation notes:
//   - Set the cancelled flag BEFORE closing cancelCh so any goroutine that
//     already woke on resultCh observes it on the post-select re-check.
//   - Delete the entry from mcpInflight immediately. Late agent replies via
//     deliverMCPResult* would otherwise still find it (their own cancelled
//     check covers concurrent delete, but evicting eagerly keeps the table
//     small under repeated kill switch / re-enable cycles).
func CancelAllMCPInflight() int {
	cancelled := 0
	mcpInflight.Range(func(key, value any) bool {
		entry, ok := value.(*mcpInflightEntry)
		if !ok {
			return true
		}
		entry.cancelCall()
		mcpInflight.Delete(key)
		cancelled++
		return true
	})
	return cancelled
}

// DeliverMCPResultForTest 暴露 deliverMCPResult 给跨包测试用：这是显式的
// "信任路径 / 不做 reporter 校验"入口，专给不关心来源的旧测试用。
// 安全敏感测试请用 DeliverMCPResultFromReporterForTest 并传入真实 reporterID。
func DeliverMCPResultForTest(res *pb.TaskResult) { deliverMCPResult(res) }

// DeliverMCPResultFromReporterForTest 暴露带 reporter 校验的投递入口给跨包
// 测试用，与生产 RequestTask 路径同语义：reporterID 必须等于 inflight 条目
// 登记的目标 serverID 才会投递。reporterID == 0 视为 "未知 reporter" 并被
// 拒绝；要绕过 reporter 校验请改用 DeliverMCPResultForTest。
func DeliverMCPResultFromReporterForTest(res *pb.TaskResult, reporterID uint64) {
	deliverMCPResultFromReporter(res, reporterID)
}

// inflightServerIDForTest 返回某个 taskID 当前挂载的目标 serverID。用于安全
// 回归测试断言 inflight 条目确实把目标 server 绑进了路由表。
// 未找到时返回 (0, false)。
func inflightServerIDForTest(taskID uint64) (uint64, bool) {
	v, ok := mcpInflight.Load(taskID)
	if !ok {
		return 0, false
	}
	entry, ok := v.(*mcpInflightEntry)
	if !ok {
		return 0, false
	}
	return entry.serverID, true
}

// deliverMCPResult 把 RequestTask 收到的 MCP-RPC TaskResult 路由到等待方。
// 找不到等待 slot（已超时被移除）则丢弃。
//
// 此变体不做 reporter 校验，仅用于不关心 reporter 的内部/测试路径。生产
// RequestTask 接收循环必须走 deliverMCPResultFromReporter，把 stream 上
// 已认证的 clientID 作为 reporter 传入。
func deliverMCPResult(res *pb.TaskResult) {
	if res == nil {
		return
	}
	v, ok := mcpInflight.Load(res.GetId())
	if !ok {
		return
	}
	entry, ok := v.(*mcpInflightEntry)
	if !ok {
		return
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.cancelled.Load() {
		return
	}
	select {
	case entry.result <- res:
	default:
	}
}

// deliverMCPResultFromReporter 是生产路径的入口：要求 reporterID 与 inflight
// 条目登记的目标 serverID 一致才投递；否则丢弃并打日志。reporterID == 0
// 视为“未知 reporter”，安全起见也丢弃。
//
// 这条校验是必要的：mcpInflight 用全局递增 taskID 做键，跨 server 共享
// 单一命名空间；如果不在投递时核对上报 agent 是 CallAgent 的目标 server，
// 任何已认证的恶意/失陷 agent 都能用猜到的 taskID 抢答其他 server 的
// MCP 调用（resultCh 容量 1，先到者覆盖真正回包）——和 commit 02129f1
// 在 cron 路径修过的攻击面同类。
func deliverMCPResultFromReporter(res *pb.TaskResult, reporterID uint64) {
	if res == nil {
		return
	}
	v, ok := mcpInflight.Load(res.GetId())
	if !ok {
		return
	}
	entry, ok := v.(*mcpInflightEntry)
	if !ok {
		return
	}
	if reporterID == 0 || entry.serverID != reporterID {
		log.Printf("NEZHA>> MCP result ignored: taskID=%d targetServerID=%d reporterID=%d",
			res.GetId(), entry.serverID, reporterID)
		return
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.cancelled.Load() {
		return
	}
	select {
	case entry.result <- res:
	default:
	}
}

// 错误类型
var (
	ErrAgentOffline = errors.New("agent offline or task stream not connected")
	ErrAgentTimeout = errors.New("agent did not respond within timeout")
)
