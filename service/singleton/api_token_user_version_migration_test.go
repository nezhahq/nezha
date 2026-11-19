package singleton

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
)

// api_tokens as it was before the user_version column existed.
type legacyAPIToken struct {
	ID        uint64 `gorm:"primaryKey"`
	UserID    uint64 `gorm:"index"`
	TokenHash string `gorm:"uniqueIndex;type:char(64)"`
	ScopesCSV string `gorm:"type:text"`
}

func (legacyAPIToken) TableName() string {
	return "api_tokens"
}

func TestInitDBFromPathBackfillsAPITokenUserVersionOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dashboard.sqlite")
	previousDB, previousConf := DB, Conf
	Conf = &ConfigClass{Config: &model.Config{}}
	closeDB := func(db *gorm.DB) {
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	}
	t.Cleanup(func() { DB, Conf = previousDB, previousConf })

	legacy, err := gorm.Open(openSQLiteDialector(path), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, legacy.AutoMigrate(&model.User{}, &legacyAPIToken{}))
	require.NoError(t, legacy.Create(&model.User{Common: model.Common{ID: 1}, Username: "rotated", TokenVersion: 5}).Error)
	require.NoError(t, legacy.Create(&model.User{Common: model.Common{ID: 2}, Username: "unchanged"}).Error)
	require.NoError(t, legacy.Create(&[]legacyAPIToken{
		{ID: 1, UserID: 1, TokenHash: "rotated-owner", ScopesCSV: model.ScopeServerRead},
		{ID: 2, UserID: 2, TokenHash: "unchanged-owner", ScopesCSV: model.ScopeServerRead},
		{ID: 3, UserID: 3, TokenHash: "deleted-owner", ScopesCSV: model.ScopeServerRead},
	}).Error)
	closeDB(legacy)

	userVersions := func() []uint64 {
		var versions []uint64
		require.NoError(t, DB.Model(&model.APIToken{}).Order("id").Pluck("user_version", &versions).Error)
		return versions
	}

	// Upgrade: tokens issued by the previous build keep working for their owners.
	require.NoError(t, InitDBFromPath(path))
	require.Equal(t, []uint64{5, 0, 0}, userVersions())

	// A later restart must not re-bind tokens that a credential change retired.
	require.NoError(t, DB.Model(&model.User{}).Where("id = ?", 1).Update("token_version", 6).Error)
	closeDB(DB)
	require.NoError(t, InitDBFromPath(path))
	require.Equal(t, []uint64{5, 0, 0}, userVersions())
	closeDB(DB)
}
