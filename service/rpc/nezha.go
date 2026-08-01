package rpc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jinzhu/copier"
	"github.com/nezhahq/nezha/pkg/tsdb"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

var _ pb.NezhaServiceServer = (*NezhaHandler)(nil)

var NezhaHandlerSingleton *NezhaHandler

// ErrRequestTaskStreamSuperseded is returned when a RequestTask result arrives
// after its stream is no longer the live stream for the authenticated server.
var ErrRequestTaskStreamSuperseded = errors.New("request task stream superseded")

type NezhaHandler struct {
	Auth                   *authHandler
	ioStreams              map[string]*ioStreamContext
	ioStreamMutex          *sync.RWMutex
	ioStreamGeneration     uint64
	ioStreamNotify         chan struct{}
	ioStreamWaitLockedHook func()
	// Capability authorization and exact stream deletion share ioStreamMutex to avoid TOCTOU.
	agentCompatCapabilities agentCompatCapabilityState
}

type serverMetricsWriter func(*tsdb.ServerMetrics) error

var writeServerMetrics serverMetricsWriter = writeServerMetricsToTSDB

func writeServerMetricsToTSDB(metrics *tsdb.ServerMetrics) error {
	if !singleton.TSDBEnabled() {
		return nil
	}
	return singleton.TSDBShared.WriteServerMetrics(metrics)
}

func NewNezhaHandler() *NezhaHandler {
	handler := &NezhaHandler{
		Auth:           &authHandler{},
		ioStreamMutex:  new(sync.RWMutex),
		ioStreams:      make(map[string]*ioStreamContext),
		ioStreamNotify: make(chan struct{}),
	}
	handler.initializeAgentCompatCapabilities()
	return handler
}

// attachRequestTaskStream resolves the server for clientID and publishes the
// task stream. It mirrors the !ok || server == nil guard the other RPC entry
// points use: the server can be deleted between CheckRequestTask and this
// lookup, in which case Get returns a nil *Server and SetTaskStream would
// panic.
func attachRequestTaskStream(clientID uint64, stream pb.NezhaService_RequestTaskServer) (*model.Server, bool) {
	server, ok := singleton.ServerShared.Get(clientID)
	if !ok || server == nil {
		return nil, false
	}
	server.SetTaskStream(stream)
	return server, true
}

// clearRequestTaskStream detaches the dropped stream from whichever *Server is
// currently published for clientID. Edit and transfer rotation publish a new
// *Server that adopts the same stream holder, so cleanup must target the live
// map entry; the captured server is only the fallback for a removed entry.
func clearRequestTaskStream(clientID uint64, captured *model.Server, stream pb.NezhaService_RequestTaskServer) {
	if current, ok := singleton.ServerShared.Get(clientID); ok && current != nil {
		current.ClearTaskStreamIfCurrent(stream)
		return
	}
	captured.ClearTaskStreamIfCurrent(stream)
}

// currentRequestTaskServer authorizes a received result against the live
// ServerShared entry. Server pointer replacement is valid when it inherited
// the same task stream holder; only a missing entry or different stream makes
// a received result stale.
func currentRequestTaskServer(clientID uint64, stream pb.NezhaService_RequestTaskServer) (*model.Server, error) {
	current, ok := singleton.ServerShared.Get(clientID)
	if !ok || current == nil || current.GetTaskStream() != stream {
		return nil, ErrRequestTaskStreamSuperseded
	}
	return current, nil
}

func (s *NezhaHandler) RequestTask(stream pb.NezhaService_RequestTaskServer) error {
	var clientID uint64
	var err error
	if clientID, err = s.Auth.CheckRequestTask(stream.Context()); err != nil {
		return err
	}

	server, ok := attachRequestTaskStream(clientID, stream)
	if !ok {
		return nil
	}
	defer clearRequestTaskStream(clientID, server, stream)
	// If a transfer is mid-flight for this server, the agent has just brought
	// up a fresh bidi stream — this is the moment to (re)deliver the
	// ApplyConfig task carrying the new owner's AgentSecret. Pushes from
	// dashboard mutation time are best-effort; this hook is the reliable
	// re-delivery point that closes the offline-during-transfer gap.
	if singleton.ServerTransferShared != nil {
		singleton.ServerTransferShared.OnAgentReconnect(clientID)
	}
	var result *pb.TaskResult
	for {
		result, err = stream.Recv()
		if err != nil {
			log.Printf("NEZHA>> RequestTask error: %v, clientID: %d\n", err, clientID)
			return err
		}
		server, err = currentRequestTaskServer(clientID, stream)
		if err != nil {
			return err
		}
		switch result.GetType() {
		case model.TaskTypeCommand:
			// 处理上报的计划任务
			cr, _ := singleton.CronShared.Get(result.GetId())
			// 任务结果 ID 来自 agent，必须确认该 cron 本应派发给当前 reporter。
			if singleton.CanReportCronResult(cr, server) {
				// 保存当前服务器状态信息
				var curServer model.Server
				copier.Copy(&curServer, server)
				if cr.PushSuccessful && result.GetSuccessful() {
					singleton.NotificationShared.SendNotification(cr.NotificationGroupID, fmt.Sprintf("[%s] %s, %s\n%s", singleton.Localizer.T("Scheduled Task Executed Successfully"),
						cr.Name, server.Name, result.GetData()), "", &curServer)
				}
				if !result.GetSuccessful() {
					singleton.NotificationShared.SendNotification(cr.NotificationGroupID, fmt.Sprintf("[%s] %s, %s\n%s", singleton.Localizer.T("Scheduled Task Executed Failed"),
						cr.Name, server.Name, result.GetData()), "", &curServer)
				}
				singleton.DB.Model(cr).Updates(model.Cron{
					LastExecutedAt: time.Now().Add(time.Second * -1 * time.Duration(result.GetDelay())),
					LastResult:     result.GetSuccessful(),
				})
			}
		case model.TaskTypeReportConfig:
			if len(server.ConfigCache) < 1 {
				if !result.GetSuccessful() {
					server.ConfigCache <- errors.New(result.Data)
					continue
				}
				server.ConfigCache <- result.Data
			}
		case model.TaskTypeServerTransferApply:
			// Authorization: TaskResult.Id is attacker-controlled. Without
			// the pending.ID == result.Id check below, agent A could cancel
			// server B's in-flight transfer by spoofing B's transfer ID —
			// same class of bug as commit 02129f1 in the cron path.
			// Successful=true here is best-effort only; the authoritative
			// verification is the agent's reconnect under the new secret.
			if singleton.ServerTransferShared == nil {
				continue
			}
			pending, ok := singleton.ServerTransferShared.LookupPending(clientID)
			if !ok || pending.ID != result.GetId() {
				log.Printf("NEZHA>> ServerTransferApply result ignored: clientID=%d reported transferID=%d but no matching pending transfer", clientID, result.GetId())
				continue
			}
			if result.GetSuccessful() {
				continue
			}
			if _, err := singleton.ServerTransferShared.MarkFailed(result.GetId(), result.GetData()); err != nil {
				log.Printf("NEZHA>> ServerTransfer MarkFailed(%d) failed: %v", result.GetId(), err)
			}
		default:
			if model.IsMCPRPCResult(result.GetType()) {
				deliverMCPResultFromReporter(result, clientID)
				continue
			}
			if model.IsServiceSentinelNeeded(result.GetType()) {
				singleton.ServiceSentinelShared.Dispatch(singleton.ReportData{
					Data:     result,
					Reporter: clientID,
				})
			}
		}
	}
}

func (s *NezhaHandler) ReportSystemState(stream pb.NezhaService_ReportSystemStateServer) error {
	clientID, err := s.Auth.Check(stream.Context())
	if err != nil {
		return err
	}
	server, ok := singleton.ServerShared.Get(clientID)
	if !ok || server == nil {
		return errors.New("server not found")
	}
	lease := server.AttachStateStream(stream)
	defer lease.Clear()
	var state *pb.State
	var stateCount uint64
	for {
		state, err = stream.Recv()
		if err != nil {
			log.Printf("NEZHA>> ReportSystemState error: %v, clientID: %d\n", err, clientID)
			return err
		}
		stateCount++
		innerState := model.PB2State(state)

		lastActive := time.Now()
		accepted := lease.UpdateStateWithSideEffect(&innerState, lastActive, func() error {
			{
				maxTemp := 0.0
				for _, t := range innerState.Temperatures {
					if t.Temperature > maxTemp {
						maxTemp = t.Temperature
					}
				}
				maxGPU := 0.0
				for _, g := range innerState.GPU {
					if g > maxGPU {
						maxGPU = g
					}
				}
				if err := writeServerMetrics(&tsdb.ServerMetrics{
					ServerID:       clientID,
					Timestamp:      lastActive,
					CPU:            innerState.CPU,
					MemUsed:        innerState.MemUsed,
					SwapUsed:       innerState.SwapUsed,
					DiskUsed:       innerState.DiskUsed,
					NetInSpeed:     innerState.NetInSpeed,
					NetOutSpeed:    innerState.NetOutSpeed,
					NetInTransfer:  innerState.NetInTransfer,
					NetOutTransfer: innerState.NetOutTransfer,
					Load1:          innerState.Load1,
					Load5:          innerState.Load5,
					Load15:         innerState.Load15,
					TCPConnCount:   innerState.TcpConnCount,
					UDPConnCount:   innerState.UdpConnCount,
					ProcessCount:   innerState.ProcessCount,
					Temperature:    maxTemp,
					Uptime:         innerState.Uptime,
					GPU:            maxGPU,
				}); err != nil {
					log.Printf("NEZHA>> Failed to write server metrics to TSDB: %v", err)
				}
			}
			return nil
		})
		if !accepted {
			return errors.New("state stream superseded")
		}

		if err := notifyStateReceived(clientID, server.UUID, lease.Generation(), stateCount); err != nil {
			return err
		}
		if err := notifyReceiptAccepted(clientID, server.UUID, lease.Generation(), stateCount); err != nil {
			return err
		}
		if err = stream.Send(&pb.Receipt{Proced: true}); err != nil {
			return err
		}
	}
}

func (s *NezhaHandler) onReportSystemInfo(c context.Context, r *pb.Host) (model.HostReportResult, error) {
	var clientID uint64
	var err error
	if clientID, err = s.Auth.Check(c); err != nil {
		return model.HostReportResult{}, err
	}
	host := model.PB2Host(r)

	server, ok := singleton.ServerShared.Get(clientID)
	if !ok || server == nil {
		return model.HostReportResult{}, errors.New("server not found")
	}

	/**
	 * 这里的 singleton 中的数据都是关机前的旧数据
	 * 当 agent 重启时，bootTime 变大，agent 端会先上报 host 信息，然后上报 state 信息
	 * 这时可以借助上报顺序的空档，立即记录停机前的数据并重置 Prev* 数据，并由接下来的 state 方法重新赋值
	 */
	return server.RuntimeHandle().ApplyHostReport(&host, time.Now(), singleton.PersistTransfer)
}

func (s *NezhaHandler) ReportSystemInfo(c context.Context, r *pb.Host) (*pb.Receipt, error) {
	if _, err := s.onReportSystemInfo(c, r); err != nil {
		return nil, err
	}
	return &pb.Receipt{Proced: true}, nil
}

func (s *NezhaHandler) ReportSystemInfo2(c context.Context, r *pb.Host) (*pb.Uint64Receipt, error) {
	result, err := s.onReportSystemInfo(c, r)
	if err != nil {
		return nil, err
	}
	if err := notifyInfo2(result.ServerID, result.UUID); err != nil {
		return nil, err
	}
	return &pb.Uint64Receipt{Data: singleton.DashboardBootTime}, nil
}
