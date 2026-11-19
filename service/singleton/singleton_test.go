package singleton

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
)

// mcp_oauth_grants.user_id carries the per-user grant cap. Grants written
// before the column existed must get the owner of their API token on upgrade.
func TestInitDBFromPathBackfillsMCPOAuthGrantUserID(t *testing.T) {
	previousDB, previousConf := DB, Conf
	Conf = &ConfigClass{Config: &model.Config{}}
	closeDB := func() {
		sqlDB, err := DB.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	}
	path := filepath.Join(t.TempDir(), "dashboard.sqlite")
	require.NoError(t, InitDBFromPath(path))
	permission := model.APIToken{UserID: 42, Name: "OAuth: client", TokenHash: model.HashAPIToken("permission")}
	require.NoError(t, DB.Create(&permission).Error)
	require.NoError(t, DB.Create(&model.MCPOAuthGrant{ID: "issued-before-upgrade", APITokenID: permission.ID, AccessHash: "access", ExpiresAt: time.Now().Add(time.Hour)}).Error)
	require.NoError(t, DB.Migrator().DropIndex(&model.MCPOAuthGrant{}, "UserID"))
	require.NoError(t, DB.Migrator().DropColumn(&model.MCPOAuthGrant{}, "UserID"))
	require.False(t, DB.Migrator().HasColumn(&model.MCPOAuthGrant{}, "user_id"))
	closeDB()

	require.NoError(t, InitDBFromPath(path))
	t.Cleanup(func() {
		closeDB()
		DB, Conf = previousDB, previousConf
	})
	var grant model.MCPOAuthGrant
	require.NoError(t, DB.First(&grant, "id = ?", "issued-before-upgrade").Error)
	require.EqualValues(t, 42, grant.UserID)
}
