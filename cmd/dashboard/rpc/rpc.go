package rpc

import (
	"context"
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

func ServeRPC() *grpc.Server {
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(getRealIp, waf))
	rpcService.NezhaHandlerSingleton = rpcService.NewNezhaHandler()
	proto.RegisterNezhaServiceServer(server, rpcService.NezhaHandlerSingleton)
	return server
}

func waf(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	realip, _ := ctx.Value(model.CtxKeyRealIP{}).(string)
	if err := model.CheckIP(singleton.DB, realip); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func getRealIp(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
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
		return handler(ctx, req)
	}

	if singleton.Conf.AgentRealIPHeader == model.ConfigUsePeerIP {
		if connectingIp == "" {
			return nil, fmt.Errorf("connecting ip not found")
		}
	} else {
		vals := metadata.ValueFromIncomingContext(ctx, singleton.Conf.AgentRealIPHeader)
		if len(vals) == 0 {
			return nil, fmt.Errorf("real ip header not found")
		}
		var err error
		ip, err = utils.GetIPFromHeader(vals[0])
		if err != nil {
			return nil, err
		}
	}

	if singleton.Conf.Debug {
		log.Printf("NEZHA>> gRPC Agent Real IP: %s, connecting IP: %s\n", ip, connectingIp)
	}

	ctx = context.WithValue(ctx, model.CtxKeyRealIP{}, ip)
	return handler(ctx, req)
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
				if server == nil || server.TaskStream == nil {
					continue
				}

				if canSendTaskToServer(task, server) {
					server.TaskStream.Send(task.PB())
				}
			}
		case model.ServiceCoverAll:
			for id, server := range singleton.ServerShared.Range {
				if server == nil || server.TaskStream == nil || task.SkipServers[id] {
					continue
				}

				if canSendTaskToServer(task, server) {
					server.TaskStream.Send(task.PB())
				}
			}
		}
	}
}

func DispatchKeepalive() {
	singleton.CronShared.AddFunc("@every 20s", func() {
		list := singleton.ServerShared.GetSortedList()
		for _, s := range list {
			if s == nil || s.TaskStream == nil {
				continue
			}
			s.TaskStream.Send(&proto.Task{Type: model.TaskTypeKeepalive})
		}
	})
}

func ServeNAT(w http.ResponseWriter, r *http.Request, natConfig *model.NAT) {
	server, _ := singleton.ServerShared.Get(natConfig.ServerID)
	if server == nil || server.TaskStream == nil {
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

	rpcService.NezhaHandlerSingleton.CreateStream(streamId)
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

	if err := server.TaskStream.Send(&proto.Task{
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

	return task.UserID == server.UserID || role.IsAdmin()
}
