package controller

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// setupDDNSUpdateDeleteTest stores a profile of user 10 that holds a DNS key
// and registers it, as createDDNS does.
func setupDDNSUpdateDeleteTest(t *testing.T) *model.DDNSProfile {
	t.Helper()
	t.Cleanup(setupTenancyTest(t))

	sqlDB, err := singleton.DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // every further connection would be another empty :memory: database

	profile := &model.DDNSProfile{Common: model.Common{UserID: 10}, Name: "home", Provider: model.ProviderWebHook,
		MaxRetries: 3, AccessID: "id", AccessSecret: "dns-key", Domains: []string{"home.example.com"}}
	require.NoError(t, singleton.DB.Create(profile).Error)
	singleton.DDNSShared.Update(profile)
	return profile
}

func updateDDNSContext(profileID uint64, form model.DDNSForm) *gin.Context {
	ctx := ctxAsMemberWithBody(10, form)
	ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(profileID, 10)}}
	return ctx
}

func requireDDNSProfileGone(t *testing.T, profileID uint64) {
	t.Helper()
	var rows int64
	require.NoError(t, singleton.DB.Model(&model.DDNSProfile{}).Count(&rows).Error)
	require.Zero(t, rows, "a deleted profile must not be stored again")
	_, registered := singleton.DDNSShared.Get(profileID)
	require.False(t, registered, "a deleted profile must not be registered for the DDNS worker again")
}

// An update must never create the row it updates: GORM's Save inserts it again,
// under its old id and with the stored credentials, when the row has been
// deleted since the handler read it.
func TestUpdateDDNSDoesNotReinsertDeletedProfile(t *testing.T) {
	profile := setupDDNSUpdateDeleteTest(t)

	// The row goes away right after updateDDNS has read it.
	var deleted bool
	require.NoError(t, singleton.DB.Callback().Query().After("gorm:query").Register("test:delete_ddns_after_read", func(tx *gorm.DB) {
		if tx.Statement.Table != "ddns" || deleted {
			return
		}
		deleted = true
		require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).Unscoped().Delete(&model.DDNSProfile{}, profile.ID).Error)
		singleton.DDNSShared.Delete([]uint64{profile.ID})
	}))

	_, err := updateDDNS(updateDDNSContext(profile.ID, model.DDNSForm{Name: "home", Provider: model.ProviderWebHook,
		MaxRetries: 1, Domains: []string{"www.example.com"}}))
	require.True(t, deleted)
	require.ErrorContains(t, err, "profile id 1 does not exist")
	requireDDNSProfileGone(t, profile.ID)
}

// raceDDNSDelete saves the profile with updateDDNS; when that update reaches
// the GORM callback registered by at, the owner's batchDeleteDDNS starts on
// another goroutine and is given time to finish before the update goes on.
// Whichever of the two is answered first, the profile must be gone once both are.
func raceDDNSDelete(t *testing.T, at func(db *gorm.DB, race func(*gorm.DB)) error) {
	t.Helper()
	profile := setupDDNSUpdateDeleteTest(t)

	var (
		raced     atomic.Bool
		deleteErr error
	)
	deleteDone := make(chan struct{})
	require.NoError(t, at(singleton.DB, func(tx *gorm.DB) {
		if tx.Statement.Table != "ddns" || !raced.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer close(deleteDone)
			_, deleteErr = batchDeleteDDNS(ctxAsMemberWithBody(10, []uint64{profile.ID}))
		}()
		select {
		case <-deleteDone:
		case <-time.After(200 * time.Millisecond):
		}
	}))

	_, _ = updateDDNS(updateDDNSContext(profile.ID, model.DDNSForm{Name: "home", Provider: model.ProviderWebHook,
		MaxRetries: 1, Domains: []string{"www.example.com"}}))
	require.True(t, raced.Load())
	<-deleteDone
	require.NoError(t, deleteErr)
	requireDDNSProfileGone(t, profile.ID)
}

func TestBatchDeleteDDNSIsNotUndoneByUpdateThatReadTheProfile(t *testing.T) {
	raceDDNSDelete(t, func(db *gorm.DB, race func(*gorm.DB)) error {
		return db.Callback().Query().After("gorm:query").Register("test:race_ddns_delete", race)
	})
}

func TestBatchDeleteDDNSIsNotUndoneByUpdateThatWroteTheProfile(t *testing.T) {
	raceDDNSDelete(t, func(db *gorm.DB, race func(*gorm.DB)) error {
		return db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("test:race_ddns_delete", race)
	})
}

// The update still writes every column of an existing profile, zero values
// included, keeps the credentials an empty form field stands for, and
// publishes what it stored.
func TestUpdateDDNSStillUpdatesExistingProfile(t *testing.T) {
	profile := setupDDNSUpdateDeleteTest(t)

	_, err := updateDDNS(updateDDNSContext(profile.ID, model.DDNSForm{Name: "renamed", Provider: model.ProviderDummy,
		MaxRetries: 1, Domains: []string{"bücher.example.com"}}))
	require.NoError(t, err)

	var stored model.DDNSProfile
	require.NoError(t, singleton.DB.First(&stored, profile.ID).Error)
	require.Equal(t, "renamed", stored.Name)
	require.Equal(t, model.ProviderDummy, stored.Provider)
	require.EqualValues(t, 1, stored.MaxRetries)
	require.Equal(t, []string{"xn--bcher-kva.example.com"}, stored.Domains)
	require.Empty(t, stored.AccessID, "an emptied field must be written")
	require.NotNil(t, stored.EnableIPv4)
	require.False(t, *stored.EnableIPv4)
	require.Equal(t, "dns-key", stored.AccessSecret, "an empty secret means keep")
	require.EqualValues(t, 10, stored.UserID)
	require.Equal(t, profile.CreatedAt.UnixNano(), stored.CreatedAt.UnixNano())

	var rows int64
	require.NoError(t, singleton.DB.Model(&model.DDNSProfile{}).Count(&rows).Error)
	require.EqualValues(t, 1, rows)
	registered, ok := singleton.DDNSShared.Get(profile.ID)
	require.True(t, ok)
	require.Equal(t, "renamed", registered.Name)
	require.Equal(t, []string{"xn--bcher-kva.example.com"}, registered.Domains)
}
