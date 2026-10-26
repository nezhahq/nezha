package singleton

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
)

func TestServiceSentinelStartupWithRetainedHistory(t *testing.T) {
	for _, serviceState := range []string{"retained", "deleted", "quarantined", "invalid_duration"} {
		t.Run(serviceState, func(t *testing.T) {
			initial := newServiceMonitorSecurityHarness(t)
			initial.Close()
			require.False(t, TSDBEnabled())

			activeService := &model.Service{Name: "active monitor", Type: model.TaskTypeTCPPing}
			historicalService := &model.Service{Name: "historical monitor", Type: model.TaskTypeHTTPGet}
			require.NoError(t, DB.Create(activeService).Error)
			require.NoError(t, DB.Create(historicalService).Error)

			year, month, day := time.Now().Date()
			today := time.Date(year, month, day, 0, 0, 0, 0, Loc)
			// Use the production writer so retained rows have the same shape as
			// ordinary aggregated history, including server_id = 0.
			historicalStatus := &serviceTaskStatus{
				lastHistoryAt: today,
				history:       serviceResponseData{Up: 7, Down: 2, Delay: 90},
			}
			initial.flushServiceHistory(historicalService.ID, historicalStatus, today.Add(15*time.Minute))
			var retainedHistory []model.ServiceHistory
			require.NoError(t, DB.Where("service_id = ?", historicalService.ID).Find(&retainedHistory).Error)
			require.Len(t, retainedHistory, 1)
			require.Zero(t, retainedHistory[0].ServerID)
			require.Equal(t, uint64(7), retainedHistory[0].Up)
			require.Equal(t, uint64(2), retainedHistory[0].Down)
			require.Equal(t, float64(90), retainedHistory[0].AvgDelay)

			require.NoError(t, DB.Create(&[]model.ServiceHistory{
				{ServiceID: activeService.ID, CreatedAt: today, Up: 5, Down: 1, AvgDelay: 10},
				{ServiceID: activeService.ID, CreatedAt: today, Up: 3, Down: 2, AvgDelay: 30},
				{ServiceID: activeService.ID, CreatedAt: today.Add(-time.Hour), Up: 4, Down: 1, AvgDelay: 15},
				{ServiceID: activeService.ID, ServerID: 100, CreatedAt: today, Up: 100, AvgDelay: 500},
			}).Error)

			switch serviceState {
			case "deleted":
				// Match the controller's hard delete without cleaning history: old
				// databases must remain loadable even if deletion cleanup changes.
				result := DB.Unscoped().Delete(&model.Service{}, "id in (?)", []uint64{historicalService.ID})
				require.NoError(t, result.Error)
				require.EqualValues(t, 1, result.RowsAffected)
			case "invalid_duration":
				require.NoError(t, DB.Model(historicalService).UpdateColumn("duration", model.MaxServiceDuration+1).Error)
			case "quarantined":
				// Legacy invalid service types are deliberately excluded at startup.
				result := DB.Model(historicalService).UpdateColumn("type", model.TaskTypeCommand)
				require.NoError(t, result.Error)
				require.EqualValues(t, 1, result.RowsAffected)
			}
			var historyCount int64
			require.NoError(t, DB.Model(&model.ServiceHistory{}).Where("service_id = ?", historicalService.ID).Count(&historyCount).Error)
			require.EqualValues(t, 1, historyCount)

			var restarted *ServiceSentinel
			var startupErr error
			require.NotPanics(t, func() {
				restarted, startupErr = NewServiceSentinel(make(chan *model.Service, 1))
			})
			if restarted != nil {
				t.Cleanup(restarted.Close)
			}
			require.NoError(t, startupErr)
			require.NotNil(t, restarted)
			require.Equal(t, &_TodayStatsOfService{Up: 8, Down: 3, Delay: 20}, restarted.serviceStatusToday[activeService.ID])
			monthly := restarted.monthlyStatus[activeService.ID]
			require.NotNil(t, monthly)
			require.Equal(t, uint64(12), monthly.TotalUp)
			require.Equal(t, uint64(4), monthly.TotalDown)

			if serviceState == "retained" {
				require.Len(t, restarted.services, 2)
				require.Equal(t, &_TodayStatsOfService{Up: 7, Down: 2, Delay: 90}, restarted.serviceStatusToday[historicalService.ID])
				require.Equal(t, uint64(7), restarted.monthlyStatus[historicalService.ID].TotalUp)
				require.Equal(t, uint64(2), restarted.monthlyStatus[historicalService.ID].TotalDown)
			} else {
				require.Len(t, restarted.services, 1)
				require.NotContains(t, restarted.services, historicalService.ID)
				require.NotContains(t, restarted.serviceStatusToday, historicalService.ID)
				require.NotContains(t, restarted.monthlyStatus, historicalService.ID)
			}
		})
	}
}
