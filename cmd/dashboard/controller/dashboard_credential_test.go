package controller

import (
	"bytes"
	"context"
	"log"
	"testing"
	"time"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func setupDashboardCredentialTest(t *testing.T) {
	t.Helper()
	originalConf, originalDB, originalParser := singleton.Conf, singleton.DB, dashboardCredentialJWTParser
	t.Cleanup(func() {
		singleton.Conf, singleton.DB, dashboardCredentialJWTParser = originalConf, originalDB, originalParser
	})

	singleton.Conf = &singleton.ConfigClass{Config: &model.Config{}}
	singleton.Conf.JWTSecretKey = "dashboard-credential-classification-test-key"
	singleton.Conf.JWTTimeout = 1

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&model.APIToken{}))
	singleton.DB = db

	// Same construction path as routers(): jwt.New(initParams()).
	authMiddleware, err := jwt.New(initParams())
	require.NoError(t, err)
	dashboardCredentialJWTParser = newDashboardCredentialJWTParser(authMiddleware)
}

// mintDashboardCredentialJWT signs a JWT with the dashboard key through
// initParams(), the same key source the production verifier uses. A negative
// timeout mints an already-expired token.
func mintDashboardCredentialJWT(t *testing.T, timeout time.Duration) string {
	t.Helper()
	params := initParams()
	params.Timeout = timeout
	mw, err := jwt.New(params)
	require.NoError(t, err)
	token, _, err := mw.TokenGenerator(nil)
	require.NoError(t, err)
	return token
}

func mintForeignJWT(t *testing.T) string {
	t.Helper()
	params := initParams()
	params.Key = []byte("foreign-signing-key-not-held-by-the-dashboard")
	mw, err := jwt.New(params)
	require.NoError(t, err)
	token, _, err := mw.TokenGenerator(nil)
	require.NoError(t, err)
	return token
}

func TestIsDashboardCredentialClassification(t *testing.T) {
	setupDashboardCredentialTest(t)

	dashboardJWT := mintDashboardCredentialJWT(t, time.Hour)
	expiredDashboardJWT := mintDashboardCredentialJWT(t, -time.Hour)
	foreignJWT := mintForeignJWT(t)

	dashboardPAT := model.APITokenPrefix + "dashboard-credential-pat"
	require.NoError(t, singleton.DB.Create(&model.APIToken{
		UserID:    1,
		Name:      "nat-classification",
		TokenHash: model.HashAPIToken(dashboardPAT),
	}).Error)

	t.Run("dashboard signed jwt is a dashboard credential", func(t *testing.T) {
		require.True(t, IsDashboardCredential("Bearer "+dashboardJWT))
	})

	t.Run("expired dashboard signed jwt is still a dashboard credential", func(t *testing.T) {
		// Signature verification ignores exp on purpose: an expired token is
		// still proof the dashboard issued it.
		require.True(t, IsDashboardCredential("Bearer "+expiredDashboardJWT))
	})

	t.Run("dashboard api token is a dashboard credential", func(t *testing.T) {
		require.True(t, IsDashboardCredential("Bearer "+dashboardPAT))
	})

	t.Run("foreign signed jwt is not a dashboard credential", func(t *testing.T) {
		require.False(t, IsDashboardCredential("Bearer "+foreignJWT))
	})

	t.Run("unknown api token is not a dashboard credential", func(t *testing.T) {
		require.False(t, IsDashboardCredential("Bearer "+model.APITokenPrefix+"unknown-to-the-dashboard"))
	})

	t.Run("random string is not a dashboard credential", func(t *testing.T) {
		require.False(t, IsDashboardCredential("Bearer random-not-a-token"))
	})

	t.Run("non bearer scheme is not a dashboard credential", func(t *testing.T) {
		require.False(t, IsDashboardCredential("Basic "+dashboardJWT))
	})

	t.Run("bearer scheme casing cannot bypass dashboard credential filtering", func(t *testing.T) {
		// HTTP auth schemes are case-insensitive, including MCP OAuth. The
		// NAT filter must strip dashboard-issued tokens for every spelling.
		require.True(t, IsDashboardCredential("bearer "+dashboardJWT))
		require.True(t, IsDashboardCredential("BEARER "+dashboardPAT))
		require.False(t, IsDashboardCredential("bearer "+foreignJWT))
	})

	t.Run("empty or missing header is not a dashboard credential", func(t *testing.T) {
		require.False(t, IsDashboardCredential(""))
		require.False(t, IsDashboardCredential("Bearer "))
	})
}

// TestIsDashboardCredentialValueClassification pins the scheme-less classifier
// that backs IsDashboardCredential and drives the ?token= query parameter and
// nz-jwt cookie channels at NAT ingress (see IsDashboardCredentialValue).
func TestIsDashboardCredentialValueClassification(t *testing.T) {
	setupDashboardCredentialTest(t)

	dashboardJWT := mintDashboardCredentialJWT(t, time.Hour)
	expiredDashboardJWT := mintDashboardCredentialJWT(t, -time.Hour)
	foreignJWT := mintForeignJWT(t)

	dashboardPAT := model.APITokenPrefix + "dashboard-credential-value-pat"
	require.NoError(t, singleton.DB.Create(&model.APIToken{
		UserID:    1,
		Name:      "nat-value-classification",
		TokenHash: model.HashAPIToken(dashboardPAT),
	}).Error)

	t.Run("dashboard signed jwt is a dashboard credential", func(t *testing.T) {
		require.True(t, IsDashboardCredentialValue(dashboardJWT))
	})
	t.Run("expired dashboard signed jwt is still a dashboard credential", func(t *testing.T) {
		require.True(t, IsDashboardCredentialValue(expiredDashboardJWT))
	})
	t.Run("dashboard api token is a dashboard credential", func(t *testing.T) {
		require.True(t, IsDashboardCredentialValue(dashboardPAT))
	})
	t.Run("foreign signed jwt is not a dashboard credential", func(t *testing.T) {
		require.False(t, IsDashboardCredentialValue(foreignJWT))
	})
	t.Run("unknown api token is not a dashboard credential", func(t *testing.T) {
		require.False(t, IsDashboardCredentialValue(model.APITokenPrefix+"unknown-to-the-dashboard"))
	})
	t.Run("random string is not a dashboard credential", func(t *testing.T) {
		require.False(t, IsDashboardCredentialValue("random-not-a-token"))
	})
	t.Run("empty value is not a dashboard credential", func(t *testing.T) {
		require.False(t, IsDashboardCredentialValue(""))
	})
	t.Run("bearer scheme is not part of the value contract", func(t *testing.T) {
		// The query and cookie channels carry the bare value; a value that
		// itself starts with "Bearer " is not something the panel issues and
		// fails signature verification.
		require.False(t, IsDashboardCredentialValue("Bearer "+dashboardJWT))
	})
}

func TestClassifyDashboardCredentialValuesBatchesPATLookup(t *testing.T) {
	setupDashboardCredentialTest(t)

	dashboardPAT := model.APITokenPrefix + "dashboard-batch-pat"
	require.NoError(t, singleton.DB.Create(&model.APIToken{
		UserID:    1,
		Name:      "nat-batch-classification",
		TokenHash: model.HashAPIToken(dashboardPAT),
	}).Error)

	queryCount := 0
	const callbackName = "test:count-dashboard-credential-batch-query"
	require.NoError(t, singleton.DB.Callback().Query().Before("gorm:query").Register(callbackName, func(*gorm.DB) {
		queryCount++
	}))
	t.Cleanup(func() { singleton.DB.Callback().Query().Remove(callbackName) })

	values := []string{
		dashboardPAT,
		model.APITokenPrefix + "unknown-one",
		dashboardPAT,
		model.APITokenPrefix + "unknown-two",
	}
	classified := ClassifyDashboardCredentialValues(context.Background(), values)

	require.Equal(t, []bool{true, false, true, false}, classified)
	require.Equal(t, 1, queryCount, "all PAT hashes in one request must use one database query")
}

func TestIsDashboardCredentialFailsClosedWhenParserUnavailable(t *testing.T) {
	setupDashboardCredentialTest(t)
	dashboardCredentialJWTParser = nil

	token := mintDashboardCredentialJWT(t, time.Hour)
	require.True(t, IsDashboardCredential("Bearer "+token))
}

func TestIsDashboardCredentialFailsClosedWhenAPITokenLookupUnavailable(t *testing.T) {
	setupDashboardCredentialTest(t)
	singleton.DB = nil

	require.True(t, IsDashboardCredential("Bearer "+model.APITokenPrefix+"unresolvable"))
}

func TestIsDashboardCredentialFailsClosedOnDatabaseError(t *testing.T) {
	setupDashboardCredentialTest(t)
	sqlDB, err := singleton.DB.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	var logs bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(originalOutput) })

	plaintext := model.APITokenPrefix + "unresolvable-after-db-failure"
	require.True(t, IsDashboardCredential("Bearer "+plaintext))
	// The credential content itself must never reach the log.
	require.NotContains(t, logs.String(), plaintext)
}
