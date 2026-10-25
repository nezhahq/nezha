package singleton

import (
	"errors"
	"sync"
	"testing"

	"github.com/nezhahq/nezha/model"
	"github.com/stretchr/testify/require"
)

func TestFrontendSelectionRollbackAndConcurrentSnapshots(t *testing.T) {
	conf := &ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{UserTemplate: "user-original", AdminTemplate: "admin-original"}}}
	user, admin := "user-installed", "admin-installed"
	writeFailure := errors.New("storage unavailable")
	require.ErrorIs(t, conf.CommitFrontendSelection(&user, &admin, func() error { return writeFailure }), writeFailure)
	selectedUser, selectedAdmin := conf.FrontendSelection()
	require.Equal(t, "user-original", selectedUser)
	require.Equal(t, "admin-original", selectedAdmin)
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Go(func() {
			for iteration := 0; iteration < 100; iteration++ {
				if err := conf.CommitFrontendSelection(&user, &admin, func() error { return nil }); err != nil {
					t.Error(err)
				}
				selectedUser, selectedAdmin := conf.FrontendSelection()
				if selectedUser != user || selectedAdmin != admin {
					t.Errorf("inconsistent selection: %s %s", selectedUser, selectedAdmin)
				}
				snapshot := conf.SettingsSnapshot()
				if snapshot.UserTemplate != user || snapshot.AdminTemplate != admin {
					t.Errorf("inconsistent settings snapshot: %+v", snapshot.ConfigDashboard)
				}
			}
		})
	}
	workers.Wait()
}
