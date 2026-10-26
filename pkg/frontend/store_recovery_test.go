package frontend

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStoreQuarantinesBrokenInstalledTheme(t *testing.T) {
	for _, metadata := range []string{"", "not json", `{"custom":false,"path":"broken-theme"}`} {
		t.Run(metadata, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "broken-theme"), 0700))
			if metadata != "" {
				require.NoError(t, os.WriteFile(filepath.Join(root, "broken-theme", "metadata.json"), []byte(metadata), 0600))
			}
			store, err := NewStore(root, []Template{{Path: "user-dist", Name: "Built in"}})
			require.NoError(t, err)
			require.Equal(t, []Template{{Path: "user-dist", Name: "Built in"}}, store.Templates())
		})
	}
}
