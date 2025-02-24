package rpc

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"time"

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

func waf(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	realip, _ := ctx.Value(model.CtxKeyRealIP{}).(string)
	if err := model.CheckIP(singleton.DB, realip); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func getRealIp(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	var ip, connectingIp string
	p, ok := peer.FromContext(ctx)
	if ok {
		addrPort, err := netip.ParseAddrPort(p.Addr.String())
		if err == nil {
			connectingIp = addrPort.Addr().String()
		}
	}
	ctx = context.WithValue(ctx, model.CtxKeyConnectingIP{}, connectingIp)

	if singleton.Conf.RealIPHeader == "" {
		return handler(ctx, req)
	}

	if singleton.Conf.RealIPHeader == model.ConfigUsePeerIP {
		if connectingIp == "" {
			return nil, fmt.Errorf("connecting ip not found")
		}
	} else {
		vals := metadata.ValueFromIncomingContext(ctx, singleton.Conf.RealIPHeader)
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

		for _, server := range singleton.ServerShared.Enumerate {
			// 如果服务器不在线，跳过这个服务器
			if server.TaskStream == nil {
				continue
			}
			// 如果此任务不可使用此服务器请求，跳过这个服务器（有些 IPv6 only 开了 NAT64 的机器请求 IPv4 总会出问题）
			if (task.Cover == model.ServiceCoverAll && task.SkipServers[server.ID]) ||
				(task.Cover == model.ServiceCoverIgnoreAll && !task.SkipServers[server.ID]) {
				continue
			}
			if task.Cover == model.ServiceCoverIgnoreAll && task.SkipServers[server.ID] {
				singleton.UserLock.RLock()
				var role uint8
				if u, ok := singleton.UserInfoMap[server.UserID]; !ok {
					role = model.RoleMember
				} else {
					role = u.Role
				}
				singleton.UserLock.RUnlock()
				if task.UserID == server.UserID || role == model.RoleAdmin {
					server.TaskStream.Send(task.PB())
				}
				continue
			}
			if task.Cover == model.ServiceCoverAll && !task.SkipServers[server.ID] {
				singleton.UserLock.RLock()
				var role uint8
				if u, ok := singleton.UserInfoMap[server.UserID]; !ok {
					role = model.RoleMember
				} else {
					role = u.Role
				}
				singleton.UserLock.RUnlock()
				if task.UserID == server.UserID || role == model.RoleAdmin {
					server.TaskStream.Send(task.PB())
				}
				continue
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
		w.Write([]byte(fmt.Sprintf("stream id error: %v", err)))
		return
	}

	rpcService.NezhaHandlerSingleton.CreateStream(streamId)
	defer rpcService.NezhaHandlerSingleton.CloseStream(streamId)

	taskData, err := utils.Json.Marshal(model.TaskNAT{
		StreamID: streamId,
		Host:     natConfig.Host,
	})
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(fmt.Sprintf("task data error: %v", err)))
		return
	}

	if err := server.TaskStream.Send(&proto.Task{
		Type: model.TaskTypeNAT,
		Data: string(taskData),
	}); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(fmt.Sprintf("send task error: %v", err)))
		return
	}

	wWrapped, err := utils.NewRequestWrapper(r, w)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(fmt.Sprintf("request wrapper error: %v", err)))
		return
	}

	if err := rpcService.NezhaHandlerSingleton.UserConnected(streamId, wWrapped); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(fmt.Sprintf("user connected error: %v", err)))
		return
	}

	rpcService.NezhaHandlerSingleton.StartStream(streamId, time.Second*10)
}
