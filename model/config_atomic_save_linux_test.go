package model

import (
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const atomicSaveTestCfg = "agent_secret_key: none\nforce_auth: true\njwt_secret_key: test\nlisten_host: 127.0.0.1\nlisten_port: 18008\n"

// A Save that cannot be written completely must leave the stored config as it
// was. A file size limit stands in for the full disk: writes beyond it fail
// with EFBIG the way they fail with ENOSPC on a volume without free blocks.
func TestFailedSaveKeepsPreviousConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(atomicSaveTestCfg), 0600); err != nil {
		t.Fatalf("write config failed: %v", err)
	}
	c := &Config{}
	if err := c.Read(path, nil); err != nil {
		t.Fatalf("read config failed: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file failed: %v", err)
	}

	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatalf("getrlimit failed: %v", err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 4096, Max: limit.Max}); err != nil {
		t.Fatalf("setrlimit failed: %v", err)
	}
	c.CustomCode = strings.Repeat("x", 16384)
	saveErr := c.Save()
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatalf("restore rlimit failed: %v", err)
	}

	if saveErr == nil {
		t.Fatal("expected Save to fail under the file size limit")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file failed: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed Save replaced config.yaml (%d bytes) with %d bytes", len(before), len(after))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read config dir failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("failed Save left %d files next to config.yaml", len(entries)-1)
	}
}

func TestSaveKeepsConfigSymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "nezha.yaml")
	if err := os.WriteFile(target, []byte(atomicSaveTestCfg), 0600); err != nil {
		t.Fatalf("write config failed: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink config failed: %v", err)
	}
	c := &Config{}
	if err := c.Read(path, nil); err != nil {
		t.Fatalf("read config failed: %v", err)
	}

	c.SiteName = "lowkick"
	if err := c.Save(); err != nil {
		t.Fatalf("save config failed: %v", err)
	}

	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Save replaced the config symlink: %v, %v", info, err)
	}
	saved, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read config file failed: %v", err)
	}
	if !strings.Contains(string(saved), "site_name: lowkick") || !strings.Contains(string(saved), "force_auth: true") {
		t.Fatalf("symlink target was not updated: %s", saved)
	}
}
