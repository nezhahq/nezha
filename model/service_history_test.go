package model

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Releases before v2.6.0 wrote agent-reported ±Inf to service_histories
// unchecked. Rows read back must still fit into a JSON response.
func TestServiceHistoryAfterFindSanitizesStoredNonFiniteDelay(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&ServiceHistory{}))

	now := time.Now()
	for _, delay := range []float64{math.Inf(1), math.Inf(-1), 12.5} {
		require.NoError(t, db.Create(&ServiceHistory{CreatedAt: now, ServiceID: 1, AvgDelay: delay, Up: 1}).Error)
	}

	var stored int64
	require.NoError(t, db.Model(&ServiceHistory{}).Where("avg_delay > 1e308 OR avg_delay < -1e308").Count(&stored).Error)
	require.Equal(t, int64(2), stored, "the rows hold non-finite delays")

	var histories []ServiceHistory
	require.NoError(t, db.Order("id").Find(&histories).Error)
	require.Len(t, histories, 3)
	require.Equal(t, []float64{0, 0, 12.5}, []float64{histories[0].AvgDelay, histories[1].AvgDelay, histories[2].AvgDelay})
	require.Equal(t, uint64(1), histories[0].Up)

	_, err = json.Marshal(histories)
	require.NoError(t, err)
}
