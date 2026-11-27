package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Version warnings carry both versions so that translated texts can show them.
func TestVersionWarningsCarryVersions(t *testing.T) {
	m := &Manager{opts: Options{Version: "v1.3.0"}}
	_, w := m.versionCheck("v1.2.3")
	require.NotNil(t, w)
	assert.Equal(t, WarnOlderVersion, w.Code)
	assert.Equal(t, "v1.2.3", w.BackupVersion)
	assert.Equal(t, "v1.3.0", w.DashboardVersion)

	m.opts.Version = "v1.2.3-selfhost.2"
	_, w = m.versionCheck("v1.2.3-selfhost.1")
	require.NotNil(t, w)
	assert.Equal(t, WarnVersionDiffers, w.Code)
	assert.Equal(t, "v1.2.3-selfhost.1", w.BackupVersion)
	assert.Equal(t, "v1.2.3-selfhost.2", w.DashboardVersion)

	m.opts.Version = "debug"
	_, w = m.versionCheck("v1.2.3")
	require.NotNil(t, w)
	assert.Equal(t, WarnVersionUnknown, w.Code)
	assert.Equal(t, "v1.2.3", w.BackupVersion)
	assert.Equal(t, "debug", w.DashboardVersion)
}
