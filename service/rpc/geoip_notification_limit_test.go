package rpc

import (
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

func TestGeoIPChangeNotificationsAreBoundedPerServer(t *testing.T) {
	flapping := requestTaskSecurityServer(7, 200, "33333333-3333-3333-3333-333333333333")
	other := requestTaskSecurityServer(8, 200, "44444444-4444-4444-4444-444444444444")
	setupRequestTaskSecurityFixture(t, []*model.Server{flapping, other}, nil, map[uint64]model.UserInfo{200: {Role: model.RoleMember}}, map[string]uint64{"synthetic-agent-secret": 200})
	originalCache := singleton.Cache
	t.Cleanup(func() { singleton.Cache = originalCache })
	singleton.Cache = cache.New(time.Minute, time.Minute)
	singleton.Conf.EnableIPChangeNotification = true
	singleton.Conf.Cover = model.ConfigCoverAll
	singleton.Conf.IPChangeNotificationGroupID = 1
	singleton.NotificationShared.InsertForTest(&model.Notification{Common: model.Common{ID: 1}, Name: "ops"})
	singleton.NotificationShared.UpdateGroup(&model.NotificationGroup{Common: model.Common{ID: 1}, Name: "ops"}, []uint64{1})
	var sent []string
	singleton.NotificationShared.SetSendForTest(func(_ *model.Notification, desc string, _ *model.Server) error {
		sent = append(sent, desc)
		return nil
	})

	handler := NewNezhaHandler()
	report := func(server *model.Server, addresses ...string) {
		ctx := requestTaskSecurityAuthedStream("synthetic-agent-secret", server.UUID).Context()
		for _, address := range addresses {
			_, err := handler.ReportGeoIP(ctx, &pb.GeoIP{Ip: &pb.IP{Ipv4: address}})
			require.NoError(t, err)
		}
	}

	// The first report only records the address; each later one is a change.
	report(flapping, "203.0.113.2", "198.51.100.1", "203.0.113.2")
	require.Len(t, sent, 2, "consecutive genuine changes must each be reported")

	const flood = 100
	for range flood {
		report(flapping, "198.51.100.1", "203.0.113.2")
	}
	afterFlood := len(sent)
	require.Less(t, afterFlood, flood, "an agent alternating its reported address must not notify on every report")
	report(flapping, "198.51.100.1", "203.0.113.2")
	require.Len(t, sent, afterFlood, "a server that spent its quota must stay quiet")

	report(other, "203.0.113.3", "198.51.100.4")
	require.Len(t, sent, afterFlood+1, "a suppressed server must not silence another server's IP change")
}
