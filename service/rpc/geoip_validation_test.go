package rpc

import (
	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestGeoIPRejectsMalformedAgentAddressBeforeUpdatingState(t *testing.T) {
	server := requestTaskSecurityServer(7, 200, "33333333-3333-3333-3333-333333333333")
	setupRequestTaskSecurityFixture(t, []*model.Server{server}, nil, map[uint64]model.UserInfo{200: {Role: model.RoleMember}}, map[string]uint64{"synthetic-agent-secret": 200})
	ctx := requestTaskSecurityAuthedStream("synthetic-agent-secret", server.UUID).Context()
	handler := NewNezhaHandler()
	for _, address := range []*pb.GeoIP{{Ip: &pb.IP{Ipv4: "not-an-ip"}}, {Ip: &pb.IP{Ipv4: "2001:db8::1"}}, {Ip: &pb.IP{Ipv6: "192.0.2.1"}}} {
		_, err := handler.ReportGeoIP(ctx, address)
		require.Error(t, err)
		actual, ok := singleton.ServerShared.Get(server.ID)
		require.True(t, ok)
		require.Equal(t, &model.GeoIP{}, actual.GeoIP, "invalid addresses must not reach state or DDNS processing")
	}
}
