package utils

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRestrictFileToOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not available on windows")
	}

	cases := []struct {
		before, after os.FileMode
	}{
		{0644, 0600},
		{0666, 0600},
		{0640, 0600},
		{0444, 0400},
		{0600, 0600},
		{0400, 0400},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
			t.Fatalf("write file: %v", err)
		}
		// chmod is not subject to the umask of the test process
		if err := os.Chmod(path, c.before); err != nil {
			t.Fatalf("chmod file: %v", err)
		}
		if err := RestrictFileToOwner(path); err != nil {
			t.Fatalf("restrict %04o: %v", c.before, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat file: %v", err)
		}
		if got := info.Mode().Perm(); got != c.after {
			t.Fatalf("mode %04o became %04o, want %04o", c.before, got, c.after)
		}
	}

	if err := RestrictFileToOwner(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing file: got %v, want a not-exist error", err)
	}
}
