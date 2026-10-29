package singleton

import (
	"errors"
	"github.com/nezhahq/nezha/pkg/frontend"
	"os"
	"path/filepath"
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

func TestFrontendRemovalSerializesWithActivation(t *testing.T) {
	for iteration := 0; iteration < 30; iteration++ {
		root := t.TempDir()
		id := "installed-user"
		directory := filepath.Join(root, id, "dist")
		require.NoError(t, os.MkdirAll(directory, 0750))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "index.html"), []byte("frontend"), 0600))
		require.NoError(t, os.WriteFile(filepath.Join(root, id, "metadata.json"), []byte(`{"path":"installed-user","custom":true}`), 0600))
		store, err := frontend.NewStore(root, []frontend.Template{{Path: "user-dist"}})
		require.NoError(t, err)
		conf := &ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{UserTemplate: "user-dist"}}, FrontendStore: store}
		start := make(chan struct{})
		activated, removed := make(chan error, 1), make(chan error, 1)
		go func() { <-start; activated <- conf.CommitFrontendSelection(&id, nil, func() error { return nil }) }()
		go func() { <-start; removed <- conf.RemoveFrontend(id, false) }()
		close(start)
		activationErr, removalErr := <-activated, <-removed
		selected, _ := conf.FrontendSelection()
		if activationErr == nil {
			require.Error(t, removalErr)
			require.Equal(t, id, selected)
			require.FileExists(t, filepath.Join(directory, "index.html"))
		} else {
			require.NoError(t, removalErr)
			require.Equal(t, "user-dist", selected)
			require.NoDirExists(t, directory)
			persisted := false
			require.Error(t, conf.CommitFrontendSelection(&id, nil, func() error { persisted = true; return nil }))
			require.False(t, persisted, "deleted selection must be rejected before persistence")
		}
	}
}
