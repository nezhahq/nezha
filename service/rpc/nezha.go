package rpc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/jinzhu/copier"
	geoipx "github.com/nezhahq/nezha/pkg/geoip"
	"github.com/nezhahq/nezha/pkg/grpcx"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

var _ pb.NezhaServiceServer = (*NezhaHandler)(nil)

var NezhaHandlerSingleton *NezhaHandler

type NezhaHandler struct {
	Auth          *authHandler
	ioStreams     map[string]*ioStreamContext
	ioStreamMutex *sync.RWMutex
}

func NewNezhaHandler() *NezhaHandler {
	return &NezhaHandler{
		Auth:          &authHandler{},
		ioStreamMutex: new(sync.RWMutex),
		ioStreams:     make(map[string]*ioStreamContext),
	}
}

func (s *NezhaHandler) RequestTask(stream pb.NezhaService_RequestTaskServer) error {
	var clientID uint64
	var err error
	if clientID, err = s.Auth.Check(stream.Context()); err != nil {
		return err
	}

	server, _ := singleton.ServerShared.Get(clientID)
	server.TaskStream = stream
	var result *pb.TaskResult
	for {
		result, err = stream.Recv()
		if err != nil {
			log.Printf("NEZHA>> RequestTask error: %v, clientID: %d\n", err, clientID)
			return err
		}
		switch result.GetType() {
		case model.TaskTypeCommand:
			// 处理上报的计划任务
			cr, _ := singleton.CronShared.Get(result.GetId())
			if cr != nil {
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
		default:
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
	var state *pb.State
	for {
		state, err = stream.Recv()
		if err != nil {
			log.Printf("NEZHA>> ReportSystemState error: %v, clientID: %d\n", err, clientID)
			return err
		}
		innerState := model.PB2State(state)

		server, ok := singleton.ServerShared.Get(clientID)
		if !ok || server == nil {
			return errors.New("server not found")
		}

		server.LastActive = time.Now()
		server.State = &innerState

		// 应对 dashboard / agent 重启的情况，如果从未记录过，先打点，等到小时时间点时入库
		if server.PrevTransferInSnapshot == 0 || server.PrevTransferOutSnapshot == 0 {
			server.PrevTransferInSnapshot = state.NetInTransfer
			server.PrevTransferOutSnapshot = state.NetOutTransfer
		}

		if err = stream.Send(&pb.Receipt{Proced: true}); err != nil {
			return err
		}
	}
}

func (s *NezhaHandler) onReportSystemInfo(c context.Context, r *pb.Host) error {
	var clientID uint64
	var err error
	if clientID, err = s.Auth.Check(c); err != nil {
		return err
	}
	host := model.PB2Host(r)

	server, ok := singleton.ServerShared.Get(clientID)
	if !ok || server == nil {
		return errors.New("server not found")
	}

	/**
	 * 这里的 singleton 中的数据都是关机前的旧数据
	 * 当 agent 重启时，bootTime 变大，agent 端会先上报 host 信息，然后上报 state 信息
	 * 这时可以借助上报顺序的空档，立即记录停机前的数据并重置 Prev* 数据，并由接下来的 state 方法重新赋值
	 */
	if !server.LastActive.IsZero() && host.BootTime > server.Host.BootTime {
		singleton.RecordTransferHourlyUsage(server)
		server.PrevTransferInSnapshot = 0
		server.PrevTransferOutSnapshot = 0
	}

	server.Host = &host
	return nil
}

func (s *NezhaHandler) ReportSystemInfo(c context.Context, r *pb.Host) (*pb.Receipt, error) {
	if err := s.onReportSystemInfo(c, r); err != nil {
		return nil, err
	}
	return &pb.Receipt{Proced: true}, nil
}

func (s *NezhaHandler) ReportSystemInfo2(c context.Context, r *pb.Host) (*pb.Uint64Receipt, error) {
	if err := s.onReportSystemInfo(c, r); err != nil {
		return nil, err
	}
	return &pb.Uint64Receipt{Data: singleton.DashboardBootTime}, nil
}

func (s *NezhaHandler) IOStream(stream pb.NezhaService_IOStreamServer) error {
	if _, err := s.Auth.Check(stream.Context()); err != nil {
		return err
	}
	id, err := stream.Recv()
	if err != nil {
		return err
	}

	// ff05ff05 是 Nezha 的魔数，用于标识流 ID
	if id == nil || len(id.Data) < 4 || (id.Data[0] != 0xff && id.Data[1] != 0x05 && id.Data[2] != 0xff && id.Data[3] == 0x05) {
		return fmt.Errorf("invalid stream id")
	}

	go func() {
		for {
			if err := stream.Send(&pb.IOStreamData{Data: []byte{}}); err != nil {
				log.Printf("NEZHA>> IOStream keepAlive error: %v\n", err)
				return
			}
			time.Sleep(time.Second * 30)
		}
	}()

	streamId := string(id.Data[4:])

	if _, err := s.GetStream(streamId); err != nil {
		return err
	}
	iw := grpcx.NewIOStreamWrapper(stream)
	if err := s.AgentConnected(streamId, iw); err != nil {
		return err
	}
	iw.Wait()
	return nil
}

func (s *NezhaHandler) ReportGeoIP(c context.Context, r *pb.GeoIP) (*pb.GeoIP, error) {
	var clientID uint64
	var err error
	if clientID, err = s.Auth.Check(c); err != nil {
		return nil, err
	}

	geoip := model.PB2GeoIP(r)
	use6 := r.GetUse6()

	if geoip.IP.IPv4Addr == "" && geoip.IP.IPv6Addr == "" {
		ip, _ := c.Value(model.CtxKeyRealIP{}).(string)
		if ip == "" {
			ip, _ = c.Value(model.CtxKeyConnectingIP{}).(string)
		}
		geoip.IP.IPv4Addr = ip
	}

	joinedIP := geoip.IP.Join()

	server, ok := singleton.ServerShared.Get(clientID)
	if !ok || server == nil {
		return nil, fmt.Errorf("server not found")
	}

	// 检查并更新DDNS
	if server.EnableDDNS && joinedIP != "" &&
		(server.GeoIP == nil || server.GeoIP.IP != geoip.IP) {
		ipv4 := geoip.IP.IPv4Addr
		ipv6 := geoip.IP.IPv6Addr

		if err := singleton.ServerShared.UpdateDDNS(server, &model.IP{IPv4Addr: ipv4, IPv6Addr: ipv6}); err != nil {
			log.Printf("NEZHA>> Failed to update DDNS for server %d: %v", err, server.ID)
		}
	}

	// 发送IP变动通知
	if server.GeoIP != nil && singleton.Conf.EnableIPChangeNotification &&
		((singleton.Conf.Cover == model.ConfigCoverAll && !singleton.Conf.IgnoredIPNotificationServerIDs[clientID]) ||
			(singleton.Conf.Cover == model.ConfigCoverIgnoreAll && singleton.Conf.IgnoredIPNotificationServerIDs[clientID])) &&
		server.GeoIP.IP.Join() != "" &&
		joinedIP != "" &&
		server.GeoIP.IP != geoip.IP {

		singleton.NotificationShared.SendNotification(singleton.Conf.IPChangeNotificationGroupID,
			fmt.Sprintf(
				"[%s] %s, %s => %s",
				singleton.Localizer.T("IP Changed"),
				server.Name, singleton.IPDesensitize(server.GeoIP.IP.Join()),
				singleton.IPDesensitize(joinedIP),
			),
			"")
	}

	// 根据内置数据库查询 IP 地理位置
	var ip string
	if geoip.IP.IPv6Addr != "" && (use6 || geoip.IP.IPv4Addr == "") {
		ip = geoip.IP.IPv6Addr
	} else {
		ip = geoip.IP.IPv4Addr
	}

	netIP := net.ParseIP(ip)
	location, err := geoipx.Lookup(netIP)
	if err != nil {
		log.Printf("NEZHA>> geoip.Lookup: %v", err)
	}
	geoip.CountryCode = location

	// 将地区码写入到 Host
	server.GeoIP = &geoip

	return &pb.GeoIP{Ip: nil, CountryCode: location, DashboardBootTime: singleton.DashboardBootTime}, nil
}
