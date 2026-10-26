package rpc

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"

	"github.com/nezhahq/nezha/model"
	geoipx "github.com/nezhahq/nezha/pkg/geoip"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

func (s *NezhaHandler) ReportGeoIP(ctx context.Context, report *pb.GeoIP) (*pb.GeoIP, error) {
	clientID, err := s.Auth.Check(ctx)
	if err != nil {
		return nil, err
	}
	geoIP := model.PB2GeoIP(report)
	if geoIP.IP.IPv4Addr == "" && geoIP.IP.IPv6Addr == "" {
		ip, _ := ctx.Value(model.CtxKeyRealIP{}).(string)
		if ip == "" {
			ip, _ = ctx.Value(model.CtxKeyConnectingIP{}).(string)
		}
		if address, parseErr := netip.ParseAddr(ip); parseErr == nil && address.Is6() && !address.Is4In6() {
			geoIP.IP.IPv6Addr = address.String()
		} else {
			geoIP.IP.IPv4Addr = ip
		}
	}
	// Validate before DDNS, notification and state side effects, including fallback addresses.
	for _, field := range []struct {
		value *string
		ipv4  bool
	}{{&geoIP.IP.IPv4Addr, true}, {&geoIP.IP.IPv6Addr, false}} {
		if *field.value == "" {
			continue
		}
		address, parseErr := netip.ParseAddr(*field.value)
		if parseErr != nil || address.Zone() != "" || address.Is4() != field.ipv4 || address.Is4In6() {
			return nil, fmt.Errorf("invalid reported IP address")
		}
		*field.value = address.String()
	}
	joinedIP := geoIP.IP.Join()
	server, ok := singleton.ServerShared.Get(clientID)
	if !ok || server == nil {
		return nil, fmt.Errorf("server not found")
	}
	if server.EnableDDNS && joinedIP != "" && (server.GeoIP == nil || server.GeoIP.IP != geoIP.IP) {
		if err := singleton.ServerShared.UpdateDDNS(server, &model.IP{IPv4Addr: geoIP.IP.IPv4Addr, IPv6Addr: geoIP.IP.IPv6Addr}); err != nil {
			log.Printf("NEZHA>> Failed to update DDNS for server %d: %v", server.ID, err)
		}
	}
	if server.GeoIP != nil && singleton.Conf.EnableIPChangeNotification &&
		((singleton.Conf.Cover == model.ConfigCoverAll && !singleton.Conf.IgnoredIPNotificationServerIDs[clientID]) ||
			(singleton.Conf.Cover == model.ConfigCoverIgnoreAll && singleton.Conf.IgnoredIPNotificationServerIDs[clientID])) &&
		server.GeoIP.IP.Join() != "" && joinedIP != "" && server.GeoIP.IP != geoIP.IP {
		singleton.NotificationShared.SendNotification(singleton.Conf.IPChangeNotificationGroupID,
			fmt.Sprintf("[%s] %s, %s => %s", singleton.Localizer.T("IP Changed"), server.Name,
				singleton.IPDesensitize(server.GeoIP.IP.Join()), singleton.IPDesensitize(joinedIP)), "")
	}
	ip := geoIP.IP.IPv4Addr
	if geoIP.IP.IPv6Addr != "" && (report.GetUse6() || ip == "") {
		ip = geoIP.IP.IPv6Addr
	}
	location, err := geoipx.Lookup(net.ParseIP(ip))
	if err != nil {
		log.Printf("NEZHA>> geoip.Lookup: %v", err)
	}
	geoIP.CountryCode = location
	server.GeoIP = &geoIP
	return &pb.GeoIP{Ip: nil, CountryCode: location, DashboardBootTime: singleton.DashboardBootTime}, nil
}
