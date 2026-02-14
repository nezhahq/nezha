package singleton

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/tsdb"
)

func newTestSentinel(serviceIDs []uint64) *ServiceSentinel {
	ss := &ServiceSentinel{
		serviceStatusToday: make(map[uint64]*_TodayStatsOfService),
		monthlyStatus:      make(map[uint64]*serviceResponseItem),
	}
	for _, id := range serviceIDs {
		ss.serviceStatusToday[id] = &_TodayStatsOfService{}
		ss.monthlyStatus[id] = &serviceResponseItem{
			service: &model.Service{Common: model.Common{ID: id}},
			ServiceResponseItem: model.ServiceResponseItem{
				Delay: &[30]float64{},
				Up:    &[30]uint64{},
				Down:  &[30]uint64{},
			},
		}
	}
	return ss
}

func setupTestDB(t *testing.T) func() {
	t.Helper()
	var err error
	DB, err = gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, DB.AutoMigrate(model.ServiceHistory{}))
	return func() { DB = nil }
}

func setupTestTSDB(t *testing.T) (*tsdb.TSDB, func()) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "tsdb_sentinel_test")
	require.NoError(t, err)
	config := &tsdb.Config{
		DataPath:           filepath.Join(tempDir, "tsdb"),
		RetentionDays:      30,
		MinFreeDiskSpaceGB: 1,
		DedupInterval:      time.Second,
	}
	db, err := tsdb.Open(config)
	require.NoError(t, err)
	TSDBShared = db
	return db, func() {
		db.Close()
		TSDBShared = nil
		os.RemoveAll(tempDir)
	}
}

func TestLoadMonthlyStatusFromDB(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)

	serviceID := uint64(1)
	ss := newTestSentinel([]uint64{serviceID})

	DB.Create(&model.ServiceHistory{
		ServiceID: serviceID,
		ServerID:  0,
		AvgDelay:  10.0,
		Up:        5,
		Down:      1,
		CreatedAt: today.Add(-25 * time.Hour),
	})
	DB.Create(&model.ServiceHistory{
		ServiceID: serviceID,
		ServerID:  0,
		AvgDelay:  20.0,
		Up:        3,
		Down:      2,
		CreatedAt: today.Add(-25 * time.Hour),
	})
	DB.Create(&model.ServiceHistory{
		ServiceID: serviceID,
		ServerID:  0,
		AvgDelay:  30.0,
		Up:        10,
		Down:      0,
		CreatedAt: today.Add(-49 * time.Hour),
	})

	ss.loadMonthlyStatusFromDB(today)

	ms := ss.monthlyStatus[serviceID]

	// day -1: index 27, two records with AvgDelay 10 and 20
	assert.InDelta(t, 15.0, ms.Delay[27], 0.01)
	assert.Equal(t, uint64(8), ms.Up[27])
	assert.Equal(t, uint64(3), ms.Down[27])

	// day -2: index 26
	assert.InDelta(t, 30.0, ms.Delay[26], 0.01)
	assert.Equal(t, uint64(10), ms.Up[26])
	assert.Equal(t, uint64(0), ms.Down[26])

	// totals
	assert.Equal(t, uint64(18), ms.TotalUp)
	assert.Equal(t, uint64(3), ms.TotalDown)

	// today (index 29) should be untouched
	assert.Equal(t, float64(0), ms.Delay[29])
	assert.Equal(t, uint64(0), ms.Up[29])
}

func TestLoadMonthlyStatusFromDB_IgnoresToday(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)

	serviceID := uint64(1)
	ss := newTestSentinel([]uint64{serviceID})

	DB.Create(&model.ServiceHistory{
		ServiceID: serviceID,
		ServerID:  0,
		AvgDelay:  50.0,
		Up:        100,
		Down:      5,
		CreatedAt: today.Add(2 * time.Hour),
	})

	ss.loadMonthlyStatusFromDB(today)

	ms := ss.monthlyStatus[serviceID]
	assert.Equal(t, uint64(0), ms.TotalUp)
	assert.Equal(t, uint64(0), ms.TotalDown)
}

func TestLoadMonthlyStatusFromDB_UnknownServiceIgnored(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)

	ss := newTestSentinel([]uint64{1})

	DB.Create(&model.ServiceHistory{
		ServiceID: 999,
		ServerID:  0,
		AvgDelay:  10.0,
		Up:        5,
		Down:      1,
		CreatedAt: today.Add(-25 * time.Hour),
	})

	ss.loadMonthlyStatusFromDB(today)

	ms := ss.monthlyStatus[uint64(1)]
	assert.Equal(t, uint64(0), ms.TotalUp)
}

func TestLoadTodayStatsFromDB(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)

	serviceID := uint64(1)
	ss := newTestSentinel([]uint64{serviceID})

	DB.Create(&model.ServiceHistory{
		ServiceID: serviceID,
		ServerID:  0,
		AvgDelay:  10.0,
		Up:        5,
		Down:      1,
		CreatedAt: today.Add(1 * time.Hour),
	})
	DB.Create(&model.ServiceHistory{
		ServiceID: serviceID,
		ServerID:  0,
		AvgDelay:  30.0,
		Up:        3,
		Down:      2,
		CreatedAt: today.Add(2 * time.Hour),
	})

	ss.loadTodayStats(today)

	st := ss.serviceStatusToday[serviceID]
	assert.Equal(t, uint64(8), st.Up)
	assert.Equal(t, uint64(3), st.Down)
	assert.InDelta(t, 20.0, st.Delay, 0.01)

	ms := ss.monthlyStatus[serviceID]
	assert.Equal(t, uint64(8), ms.TotalUp)
	assert.Equal(t, uint64(3), ms.TotalDown)
}

func TestLoadMonthlyStatusFromTSDB(t *testing.T) {
	db, cleanup := setupTestTSDB(t)
	defer cleanup()

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)

	serviceID := uint64(1)
	services := []*model.Service{{Common: model.Common{ID: serviceID}}}
	ss := newTestSentinel([]uint64{serviceID})

	yesterday := today.Add(-25 * time.Hour)
	for i := 0; i < 5; i++ {
		ts := yesterday.Add(time.Duration(i) * time.Minute)
		require.NoError(t, db.WriteServiceMetrics(&tsdb.ServiceMetrics{
			ServiceID:  serviceID,
			ServerID:   1,
			Timestamp:  ts,
			Delay:      float64(10 + i),
			Successful: true,
		}))
	}
	for i := 0; i < 3; i++ {
		require.NoError(t, db.WriteServiceMetrics(&tsdb.ServiceMetrics{
			ServiceID:  serviceID,
			ServerID:   1,
			Timestamp:  yesterday.Add(time.Duration(i+10) * time.Minute),
			Delay:      float64(20 + i),
			Successful: false,
		}))
	}

	db.Flush()

	ss.loadMonthlyStatusFromTSDB(services, today)

	ms := ss.monthlyStatus[serviceID]
	// day -1: dayIndex 28
	assert.Equal(t, uint64(5), ms.Up[28])
	assert.Equal(t, uint64(3), ms.Down[28])
	assert.Equal(t, uint64(5), ms.TotalUp)
	assert.Equal(t, uint64(3), ms.TotalDown)
	assert.Greater(t, ms.Delay[28], float64(0))

	// today (index 29) should be untouched
	assert.Equal(t, uint64(0), ms.Up[29])
}

func TestLoadTodayStatsFromTSDB(t *testing.T) {
	db, cleanup := setupTestTSDB(t)
	defer cleanup()

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)

	serviceID := uint64(1)
	ss := newTestSentinel([]uint64{serviceID})

	now := time.Now()
	for i := 0; i < 4; i++ {
		ts := now.Add(-time.Duration(i) * time.Minute)
		require.NoError(t, db.WriteServiceMetrics(&tsdb.ServiceMetrics{
			ServiceID:  serviceID,
			ServerID:   1,
			Timestamp:  ts,
			Delay:      float64(10 + i),
			Successful: true,
		}))
	}
	for i := 0; i < 2; i++ {
		ts := now.Add(-time.Duration(i+10) * time.Minute)
		require.NoError(t, db.WriteServiceMetrics(&tsdb.ServiceMetrics{
			ServiceID:  serviceID,
			ServerID:   1,
			Timestamp:  ts,
			Delay:      0,
			Successful: false,
		}))
	}

	db.Flush()

	ss.loadTodayStats(today)

	st := ss.serviceStatusToday[serviceID]
	assert.Greater(t, st.Up, uint64(0))
	assert.Greater(t, st.Down, uint64(0))

	ms := ss.monthlyStatus[serviceID]
	assert.Equal(t, st.Up, ms.TotalUp)
	assert.Equal(t, st.Down, ms.TotalDown)
}

func TestLoadMonthlyStatusFromTSDB_NoDoubleCountToday(t *testing.T) {
	db, cleanup := setupTestTSDB(t)
	defer cleanup()

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)

	serviceID := uint64(1)
	services := []*model.Service{{Common: model.Common{ID: serviceID}}}
	ss := newTestSentinel([]uint64{serviceID})

	now := time.Now()
	for i := 0; i < 5; i++ {
		require.NoError(t, db.WriteServiceMetrics(&tsdb.ServiceMetrics{
			ServiceID:  serviceID,
			ServerID:   1,
			Timestamp:  now.Add(-time.Duration(i) * time.Minute),
			Delay:      10.0,
			Successful: true,
		}))
	}
	db.Flush()

	ss.loadMonthlyStatusFromTSDB(services, today)
	totalAfterMonthly := ss.monthlyStatus[serviceID].TotalUp

	ss.loadTodayStats(today)
	totalAfterToday := ss.monthlyStatus[serviceID].TotalUp

	assert.Equal(t, totalAfterMonthly+ss.serviceStatusToday[serviceID].Up, totalAfterToday)
}
