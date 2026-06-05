package rpc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"time"

	"github.com/goccy/go-json"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	"github.com/hashicorp/go-uuid"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/proto"
	rpcService "github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
)

// SetMCPKillSwitchObserver re-exports the service/rpc hook so cmd/dashboard
// can wire singleton.Conf.EnableMCP without importing the inner rpc package
// (cmd/dashboard already imports cmd/dashboard/rpc for ServeRPC).
func SetMCPKillSwitchObserver(fn func() bool) {
	rpcService.SetMCPKillSwitchObserver(fn)
}

func ServeRPC() *grpc.Server {
	// Streaming RPCs (RequestTask, IOStream) need the same real-IP + WAF
	// gate as unary calls; without the stream interceptors authHandler.check
	// sees an empty real IP, so brute-force BlockIP counters never key on a
	// source and the WAF block table is bypassed at the stream entrypoint.
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(getRealIp, waf),
		grpc.ChainStreamInterceptor(getRealIpStream, wafStream),
	)
	rpcService.NezhaHandlerSingleton = rpcService.NewNezhaHandler()
	// Install the IOStream revocation hook so ServerTransferShared can tear
	// down terminal/FM/NAT sessions held by the previous owner on every
	// ownership rotation (Register/revertTransition/OnServersDeleted).
	singleton.ServerTransferStreamRevocationHook = rpcService.NezhaHandlerSingleton.RevokeStreamsForServer
	proto.RegisterNezhaServiceServer(server, rpcService.NezhaHandlerSingleton)
	return server
}

func ctxWithRealIP(ctx context.Context) (context.Context, error) {
	var ip, connectingIp string
	p, ok := peer.FromContext(ctx)
	if ok {
		addrPort, err := netip.ParseAddrPort(p.Addr.String())
		if err == nil {
			connectingIp = addrPort.Addr().String()
		}
	}
	ctx = context.WithValue(ctx, model.CtxKeyConnectingIP{}, connectingIp)

	if singleton.Conf.AgentRealIPHeader == "" {
		return ctx, nil
	}

	if singleton.Conf.AgentRealIPHeader == model.ConfigUsePeerIP {
		if connectingIp == "" {
			return ctx, fmt.Errorf("connecting ip not found")
		}
		// Peer-IP mode: peer IP is the real IP. Leaving ip="" makes
		// CheckIP/BlockIP short-circuit on empty IP, disabling the WAF.
		ip = connectingIp
	} else {
		vals := metadata.ValueFromIncomingContext(ctx, singleton.Conf.AgentRealIPHeader)
		if len(vals) == 0 {
			return ctx, fmt.Errorf("real ip header not found")
		}
		var err error
		ip, err = utils.GetIPFromHeader(vals[0])
		if err != nil {
			return ctx, err
		}
	}

	if singleton.Conf.Debug {
		log.Printf("NEZHA>> gRPC Agent Real IP: %s, connecting IP: %s\n", ip, connectingIp)
	}

	return context.WithValue(ctx, model.CtxKeyRealIP{}, ip), nil
}

func waf(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	realip, _ := ctx.Value(model.CtxKeyRealIP{}).(string)
	if err := model.CheckIP(singleton.DB, realip); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func getRealIp(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	ctx, err := ctxWithRealIP(ctx)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// realIPServerStream overrides Context() so stream handlers and
// authHandler.check observe the resolved real IP, like the unary path.
type realIPServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *realIPServerStream) Context() context.Context { return s.ctx }

func getRealIpStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, err := ctxWithRealIP(ss.Context())
	if err != nil {
		return err
	}
	return handler(srv, &realIPServerStream{ServerStream: ss, ctx: ctx})
}

func wafStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	realip, _ := ss.Context().Value(model.CtxKeyRealIP{}).(string)
	if err := model.CheckIP(singleton.DB, realip); err != nil {
		return err
	}
	return handler(srv, ss)
}

func DispatchTask(serviceSentinelDispatchBus <-chan *model.Service) {
	for task := range serviceSentinelDispatchBus {
		if task == nil {
			continue
		}

		switch task.Cover {
		case model.ServiceCoverIgnoreAll:
			for id, enabled := range task.SkipServers {
				if !enabled {
					continue
				}

				server, _ := singleton.ServerShared.Get(id)
				if server == nil {
					continue
				}
				if !canSendTaskToServer(task, server) {
					continue
				}
				// SendTask 走 holder-scoped send mutex，避免与 cron /
				// server-transfer / MCP CallAgent / fs.transfer 等并发
				// SendMsg 同一 RequestTask stream。
				if err := server.SendTask(task.PB()); err != nil &&
					!errors.Is(err, model.ErrTaskStreamOffline) {
					log.Printf("NEZHA>> DispatchTask send error (server=%d): %v", id, err)
				}
			}
		case model.ServiceCoverAll:
			// 快照后逐个 SendTask，不在 ServerShared 的 listMu.RLock 内做阻塞
			// gRPC：否则一个卡死 agent 会拖死需要写锁的 server 生命周期操作。
			for id, server := range singleton.ServerShared.GetList() {
				if server == nil || task.SkipServers[id] {
					continue
				}
				if !canSendTaskToServer(task, server) {
					continue
				}
				if err := server.SendTask(task.PB()); err != nil &&
					!errors.Is(err, model.ErrTaskStreamOffline) {
					log.Printf("NEZHA>> DispatchTask send error (server=%d): %v", id, err)
				}
			}
		}
	}
}

func DispatchKeepalive() {
	singleton.CronShared.AddFunc("@every 20s", func() {
		list := singleton.ServerShared.GetSortedList()
		for _, s := range list {
			if s == nil {
				continue
			}
			if err := s.SendTask(&proto.Task{Type: model.TaskTypeKeepalive}); err != nil &&
				!errors.Is(err, model.ErrTaskStreamOffline) {
				log.Printf("NEZHA>> Keepalive send error (server=%d): %v", s.ID, err)
			}
		}
	})
}

func ServeNAT(w http.ResponseWriter, r *http.Request, natConfig *model.NAT) {
	server, _ := singleton.ServerShared.Get(natConfig.ServerID)
	if server == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("server not found or not connected"))
		return
	}
	if server.GetTaskStream() == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("server not found or not connected"))
		return
	}

	streamId, err := uuid.GenerateUUID()
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(fmt.Appendf(nil, "stream id error: %v", err))
		return
	}

	// NAT streams are anonymous HTTP-facing tunnels; they are NOT reachable
	// via /ws/terminal or /ws/file (which check stream ownership), so the
	// creator user ID does not need to identify a real user. The targetServerID
	// IS required though — the receiving agent must prove it is the server the
	// NAT config addressed, otherwise any agent that snoops the streamId can
	// answer NAT traffic on behalf of an unrelated host.
	if err := rpcService.NezhaHandlerSingleton.CreateStream(streamId, 0, server.ID); err != nil {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(fmt.Appendf(nil, "stream limit: %v", err))
		return
	}
	defer rpcService.NezhaHandlerSingleton.CloseStream(streamId)

	taskData, err := json.Marshal(model.TaskNAT{
		StreamID: streamId,
		Host:     natConfig.Host,
	})
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(fmt.Appendf(nil, "task data error: %v", err))
		return
	}

	if err := server.SendTask(&proto.Task{
		Type: model.TaskTypeNAT,
		Data: string(taskData),
	}); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(fmt.Appendf(nil, "send task error: %v", err))
		return
	}

	wWrapped, err := utils.NewRequestWrapper(r, w)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(fmt.Appendf(nil, "request wrapper error: %v", err))
		return
	}

	if err := rpcService.NezhaHandlerSingleton.UserConnected(streamId, wWrapped); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(fmt.Appendf(nil, "user connected error: %v", err))
		return
	}

	rpcService.NezhaHandlerSingleton.StartStream(streamId, time.Second*10)
}

func canSendTaskToServer(task *model.Service, server *model.Server) bool {
	var role model.Role
	singleton.UserLock.RLock()
	if u, ok := singleton.UserInfoMap[task.UserID]; !ok {
		role = model.RoleMember
	} else {
		role = u.Role
	}
	singleton.UserLock.RUnlock()

	return task.UserID == server.GetUserID() || role.IsAdmin()
}
