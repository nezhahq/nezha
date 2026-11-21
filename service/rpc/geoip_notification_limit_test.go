package rpc

import (
	"sync"
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
	// Notifications are sent from their own goroutine so that a slow webhook
	// cannot stall the report; wait for the count to settle before reading it.
	var sentMu sync.Mutex
	var sent []string
	singleton.NotificationShared.SetSendForTest(func(_ *model.Notification, desc string, _ *model.Server) error {
		sentMu.Lock()
		defer sentMu.Unlock()
		sent = append(sent, desc)
		return nil
	})
	settledSentCount := func() int {
		count, stableSince := -1, time.Now()
		for time.Since(stableSince) < 100*time.Millisecond {
			sentMu.Lock()
			current := len(sent)
			sentMu.Unlock()
			if current != count {
				count, stableSince = current, time.Now()
			}
			time.Sleep(5 * time.Millisecond)
		}
		return count
	}

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
	require.Equal(t, 2, settledSentCount(), "consecutive genuine changes must each be reported")

	const flood = 100
	for range flood {
		report(flapping, "198.51.100.1", "203.0.113.2")
	}
	afterFlood := settledSentCount()
	require.Less(t, afterFlood, flood, "an agent alternating its reported address must not notify on every report")
	report(flapping, "198.51.100.1", "203.0.113.2")
	require.Equal(t, afterFlood, settledSentCount(), "a server that spent its quota must stay quiet")

	report(other, "203.0.113.3", "198.51.100.4")
	require.Equal(t, afterFlood+1, settledSentCount(), "a suppressed server must not silence another server's IP change")

	// A webhook can take minutes. Sending from the report handler made the
	// agent's report time out, and its retry triggered the DDNS update and the
	// notification again.
	webhookStarted, releaseWebhook := make(chan struct{}, 1), make(chan struct{})
	defer close(releaseWebhook)
	singleton.NotificationShared.SetSendForTest(func(*model.Notification, string, *model.Server) error {
		webhookStarted <- struct{}{}
		<-releaseWebhook
		return nil
	})
	reported := make(chan struct{})
	go func() {
		report(other, "203.0.113.5")
		close(reported)
	}()
	select {
	case <-webhookStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("test setup: the IP change was not notified")
	}
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("the report waited for the notification webhook")
	}
}
