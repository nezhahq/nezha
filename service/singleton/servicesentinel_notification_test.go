package singleton

import (
	"testing"
	"time"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/patrickmn/go-cache"
)

func TestNotifyCheckFormatsProbeErrorWhenPresent(t *testing.T) {
	oldCache, oldLocalizer, oldNotification := Cache, Localizer, NotificationShared
	Cache = cache.New(time.Minute, time.Minute)
	t.Cleanup(func() {
		Cache, Localizer, NotificationShared = oldCache, oldLocalizer, oldNotification
	})

	notification := &model.Notification{Common: model.Common{ID: 1}, Name: "hook"}
	nc := newNotificationClassWithItems(notification)
	nc.UpdateGroup(&model.NotificationGroup{Common: model.Common{ID: 2}, Name: "service"}, []uint64{notification.ID})
	NotificationShared = nc
	delivered := make(chan string, 1)
	nc.sendForTest = func(_ *model.Notification, message string, _ *model.Server) error {
		delivered <- message
		return nil
	}

	service := &model.Service{Common: model.Common{ID: 3}, Name: "API", Notify: true, NotificationGroupID: 2}
	report := &ReportData{Reporter: 4}
	servers := map[uint64]*model.Server{4: {Common: model.Common{ID: 4}, Name: "probe"}}

	tests := []struct {
		name       string
		language   string
		probeError string
		want       string
	}{
		{"recovery", "en_US", "", "[Good] API Reporter: probe"},
		{"probe still reports an error", "en_US", "connection timed out", "[Good] API Reporter: probe, Error: connection timed out"},
		{"translated recovery", "zh_CN", "", "[正常] API 报告服务：probe"},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Localizer = i18n.NewLocalizer(tt.language, domain, "translations", i18n.Translations)
			notifyCheck(report, servers, service, &pb.TaskResult{Id: service.ID, Data: tt.probeError}, StatusDown, StatusGood, uint64(index))
			nc.deliveryMu.Lock()
			worker := nc.serviceDelivery[service.ID]
			nc.deliveryMu.Unlock()
			if worker != nil {
				t.Cleanup(func() {
					nc.CancelServiceState(service.ID)
					<-worker.done
				})
			}
			select {
			case got := <-delivered:
				if got != tt.want {
					t.Fatalf("notification = %q, want %q", got, tt.want)
				}
			case <-time.After(time.Second):
				t.Fatal("service notification was not delivered")
			}
		})
	}
}
