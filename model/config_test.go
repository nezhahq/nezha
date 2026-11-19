package model

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadConfig(t *testing.T) {
	t.Run("ReadEmptyConfig", func(t *testing.T) {
		file := newTempConfig(t, "")
		c := &Config{}

		if err := c.Read(file, nil); err != nil {
			t.Fatalf("read empty config failed: %v", err)
		}

		testFields := []struct {
			Name  string
			Value any
			Cond  bool
		}{
			{"jwt_secret_key", c.JWTSecretKey, c.JWTSecretKey != ""},
			{"user_template", c.UserTemplate, c.UserTemplate == "user-dist"},
			{"admin_template", c.AdminTemplate, c.AdminTemplate == "admin-dist"},
			{"agent_secret_key", c.AgentSecretKey, c.AgentSecretKey != ""},
			{"allow_jwt_ip_change", c.AllowJWTIPChange, !c.AllowJWTIPChange},
		}

		for _, field := range testFields {
			if !field.Cond {
				t.Fatalf("%s did not passed check, value: %v", field.Name, field.Value)
			}
		}

		os.Remove(file)
	})

	t.Run("ReadFile", func(t *testing.T) {
		const testCfg = "jwt_secret_key: test\nuser_template: um\nadmin_template: am\nagent_secret_key: none\nsite_name: lowkick\nallow_jwt_ip_change: true"

		var testFrontendTemplates = []FrontendTemplate{
			{Path: "um"},
			{Path: "am", IsAdmin: true},
		}
		file := newTempConfig(t, testCfg)
		c := &Config{}

		if err := c.Read(file, testFrontendTemplates); err != nil {
			t.Fatalf("read config failed: %v", err)
		}

		testFields := []struct {
			Name  string
			Value any
			Cond  bool
		}{
			{"jwt_secret_key", c.JWTSecretKey, c.JWTSecretKey == "test"},
			{"user_template", c.UserTemplate, c.UserTemplate == "um"},
			{"admin_template", c.AdminTemplate, c.AdminTemplate == "am"},
			{"agent_secret_key", c.AgentSecretKey, c.AgentSecretKey == "none"},
			{"site_name", c.SiteName, c.SiteName == "lowkick"},
			{"allow_jwt_ip_change", c.AllowJWTIPChange, c.AllowJWTIPChange},
		}

		for _, field := range testFields {
			if !field.Cond {
				t.Fatalf("%s did not passed check, value: %v", field.Name, field.Value)
			}
		}

		os.Remove(file)
	})

	t.Run("ReadEnv", func(t *testing.T) {
		os.Setenv("NZ_JWTSECRETKEY", "test")
		os.Setenv("NZ_USERTEMPLATE", "um")
		os.Setenv("NZ_ADMINTEMPLATE", "am")
		os.Setenv("NZ_AGENTSECRETKEY", "none")
		os.Setenv("NZ_HTTPS_LISTENPORT", "9876")

		var testFrontendTemplates = []FrontendTemplate{
			{Path: "um"},
			{Path: "am", IsAdmin: true},
		}
		file := newTempConfig(t, "")
		c := &Config{}

		if err := c.Read(file, testFrontendTemplates); err != nil {
			t.Fatalf("read empty config failed: %v", err)
		}

		testFields := []struct {
			Name  string
			Value any
			Cond  bool
		}{
			{"jwt_secret_key", c.JWTSecretKey, c.JWTSecretKey == "test"},
			{"user_template", c.UserTemplate, c.UserTemplate == "um"},
			{"admin_template", c.AdminTemplate, c.AdminTemplate == "am"},
			{"agent_secret_key", c.AgentSecretKey, c.AgentSecretKey == "none"},
			{"https.listenport", c.HTTPS.ListenPort, c.HTTPS.ListenPort == 9876},
		}

		for _, field := range testFields {
			if !field.Cond {
				t.Fatalf("%s did not passed check, value: %v", field.Name, field.Value)
			}
		}

		os.Remove(file)
	})

	t.Run("ReadEnvFile", func(t *testing.T) {
		t.Setenv("NZ_JWTSECRETKEY", "test1")
		t.Setenv("NZ_USERTEMPLATE", "um1")
		t.Setenv("NZ_ADMINTEMPLATE", "am1")
		t.Setenv("NZ_AGENTSECRETKEY", "none1")
		t.Setenv("NZ_SITENAME", "lowkick1")

		const testCfg = "jwt_secret_key: test\nuser_template: um\nadmin_template: am\nagent_secret_key: none\nsite_name: lowkick"

		var testFrontendTemplates = []FrontendTemplate{
			{Path: "um"},
			{Path: "am", IsAdmin: true},
			{Path: "um1"},
			{Path: "am1", IsAdmin: true},
		}
		file := newTempConfig(t, testCfg)
		c := &Config{}

		if err := c.Read(file, testFrontendTemplates); err != nil {
			t.Fatalf("read empty config failed: %v", err)
		}

		testFields := []struct {
			Name  string
			Value any
			Cond  bool
		}{
			{"jwt_secret_key", c.JWTSecretKey, c.JWTSecretKey == "test1"},
			{"jwt_secret_from_env", c.jwtSecretFromEnv, c.jwtSecretFromEnv},
			{"user_template", c.UserTemplate, c.UserTemplate == "um1" || c.UserTemplate == "um"},
			{"admin_template", c.AdminTemplate, c.AdminTemplate == "am1" || c.AdminTemplate == "am"},
			{"agent_secret_key", c.AgentSecretKey, c.AgentSecretKey == "none" || c.AgentSecretKey == "none1"},
			{"site_name", c.SiteName, c.SiteName == "lowkick" || c.SiteName == "lowkick1"},
		}

		for _, field := range testFields {
			if !field.Cond {
				t.Fatalf("%s did not passed check, value: %v", field.Name, field.Value)
			}
		}

		os.Remove(file)
	})
}

func TestRotateJWTSecretKeyIfNeeded(t *testing.T) {
	tests := []struct {
		name               string
		initialMarker      string
		currentVersion     string
		wantRotated        bool
		wantStoredVersion  string
		wantSecretChanged  bool
		wantSavedConfigKey bool
	}{
		{
			name:               "empty marker rotates leaked secret",
			currentVersion:     "v2.0.13",
			wantRotated:        true,
			wantStoredVersion:  "v2.0.13",
			wantSecretChanged:  true,
			wantSavedConfigKey: true,
		},
		{
			name:               "old marker rotates leaked secret",
			initialMarker:      "v2.0.12",
			currentVersion:     "v2.0.14",
			wantRotated:        true,
			wantStoredVersion:  "v2.0.14",
			wantSecretChanged:  true,
			wantSavedConfigKey: true,
		},
		{
			name:               "threshold marker keeps secret and advances marker",
			initialMarker:      "v2.0.13",
			currentVersion:     "v2.0.14",
			wantStoredVersion:  "v2.0.14",
			wantSavedConfigKey: true,
		},
		{
			name:              "current marker keeps secret",
			initialMarker:     "v2.0.14",
			currentVersion:    "v2.0.14",
			wantStoredVersion: "v2.0.14",
		},
		{
			name:           "debug version skips rotation and marker update",
			currentVersion: "debug",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := newTempConfig(t, "")
			t.Cleanup(func() { os.Remove(file) })

			c := &Config{
				JWTSecretKey:                   "leaked-secret",
				JWTSecretKeyLastRotatedVersion: tt.initialMarker,
				filePath:                       file,
			}

			rotated, err := c.RotateJWTSecretKeyIfNeeded(tt.currentVersion)
			if err != nil {
				t.Fatalf("rotate jwt secret key failed: %v", err)
			}
			if rotated != tt.wantRotated {
				t.Fatalf("rotated = %v, want %v", rotated, tt.wantRotated)
			}
			if c.JWTSecretKeyLastRotatedVersion != tt.wantStoredVersion {
				t.Fatalf("jwt secret key marker = %q, want %q", c.JWTSecretKeyLastRotatedVersion, tt.wantStoredVersion)
			}
			secretChanged := c.JWTSecretKey != "leaked-secret"
			if secretChanged != tt.wantSecretChanged {
				t.Fatalf("secret changed = %v, want %v", secretChanged, tt.wantSecretChanged)
			}

			saved, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read saved config: %v", err)
			}
			hasMarker := strings.Contains(string(saved), "jwt_secret_key_last_rotated_version")
			if hasMarker != tt.wantSavedConfigKey {
				t.Fatalf("saved marker present = %v, want %v, config = %s", hasMarker, tt.wantSavedConfigKey, saved)
			}
		})
	}
}

// Mirrors the upstream single-block declaration so iota lines up exactly:
// ConfigUsePeerIP occupies iota=0 (as a typed string), ConfigCoverAll=1,
// ConfigCoverIgnoreAll=2. Pins persisted `cover` semantics.
const (
	originalConfigUsePeerIP = "NZ::Use-Peer-IP"
	originalConfigCoverAll  = iota
	originalConfigCoverIgnoreAll
)

func TestConfigCoverConstantValues(t *testing.T) {
	if ConfigUsePeerIP != originalConfigUsePeerIP {
		t.Fatalf("ConfigUsePeerIP = %q, want %q", ConfigUsePeerIP, originalConfigUsePeerIP)
	}
	if ConfigCoverAll != originalConfigCoverAll {
		t.Fatalf("ConfigCoverAll = %d, want original value %d", ConfigCoverAll, originalConfigCoverAll)
	}
	if ConfigCoverIgnoreAll != originalConfigCoverIgnoreAll {
		t.Fatalf("ConfigCoverIgnoreAll = %d, want original value %d", ConfigCoverIgnoreAll, originalConfigCoverIgnoreAll)
	}
}

// A config.yaml that exists before the first start (seeded by an installer,
// bind-mounted, left 0644 by an older version) must not stay readable by other
// local users once jwt_secret_key and agent_secret_key are stored in it.
func TestConfigFileIsRestrictedToOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not available on windows")
	}
	t.Setenv(JWTSecretEnvKey, "")

	worldReadableConfig := func(t *testing.T, cfg string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
			t.Fatalf("write config failed: %v", err)
		}
		// chmod is not subject to the umask of the test process
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatalf("chmod config failed: %v", err)
		}
		return path
	}
	assertPrivate := func(t *testing.T, path string) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat config failed: %v", err)
		}
		if mode := info.Mode().Perm(); mode&0077 != 0 {
			t.Fatalf("config holds jwt_secret_key and agent_secret_key but has mode %04o", mode)
		}
	}

	t.Run("FirstStartGeneratesSecrets", func(t *testing.T) {
		path := worldReadableConfig(t, "listen_port: 8008\n")
		c := &Config{}
		if err := c.Read(path, nil); err != nil {
			t.Fatalf("read config failed: %v", err)
		}
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read saved config failed: %v", err)
		}
		if !strings.Contains(string(saved), "jwt_secret_key: "+c.JWTSecretKey) {
			t.Fatal("generated jwt_secret_key was not persisted")
		}
		assertPrivate(t, path)
	})

	t.Run("ExistingSecretsAreNotRewritten", func(t *testing.T) {
		path := worldReadableConfig(t, "jwt_secret_key: test\nagent_secret_key: none\n")
		c := &Config{}
		if err := c.Read(path, nil); err != nil {
			t.Fatalf("read config failed: %v", err)
		}
		assertPrivate(t, path)
	})

	t.Run("SaveAndPatch", func(t *testing.T) {
		path := worldReadableConfig(t, "jwt_secret_key: test\nagent_secret_key: none\n")
		c := &Config{}
		if err := c.Read(path, nil); err != nil {
			t.Fatalf("read config failed: %v", err)
		}

		if err := os.Chmod(path, 0644); err != nil {
			t.Fatalf("chmod config failed: %v", err)
		}
		if err := c.Save(); err != nil {
			t.Fatalf("save config failed: %v", err)
		}
		assertPrivate(t, path)

		if err := os.Chmod(path, 0644); err != nil {
			t.Fatalf("chmod config failed: %v", err)
		}
		if err := c.patchYAMLField("jwt_secret_key", "patched"); err != nil {
			t.Fatalf("patch config failed: %v", err)
		}
		assertPrivate(t, path)
	})
}

// config.yaml overrides NZ_* variables, so a setting an administrator switched
// off or cleared must stay that way after a restart with the variable still set.
func TestSavedEmptySettingsOverrideEnvironment(t *testing.T) {
	t.Setenv("NZ_ENABLEMCP", "true")
	t.Setenv("NZ_ALLOWJWTIPCHANGE", "true")
	t.Setenv("NZ_NATMEMBERDOMAINS", "tunnel.example.com")
	t.Setenv("NZ_RESERVEDHOSTS", "admin.example.com")
	t.Setenv(JWTSecretEnvKey, "secret-from-environment")

	t.Run("cleared settings survive a restart", func(t *testing.T) {
		file := newTempConfig(t, "agent_secret_key: none\n")
		t.Cleanup(func() { os.Remove(file) })

		c := &Config{}
		if err := c.Read(file, nil); err != nil {
			t.Fatalf("read config failed: %v", err)
		}
		if !c.MCPEnabled() || !c.JWTIPChangeAllowed() || len(c.NATMemberDomains) != 1 || c.ReservedHosts == "" {
			t.Fatalf("environment not applied: mcp=%v ip_change=%v nat=%v reserved=%q",
				c.MCPEnabled(), c.JWTIPChangeAllowed(), c.NATMemberDomains, c.ReservedHosts)
		}
		c.SetMCPEnabled(false)
		c.SetJWTIPChangeAllowed(false)
		c.NATMemberDomains = nil
		c.ReservedHosts = ""
		if err := c.Save(); err != nil {
			t.Fatalf("save config failed: %v", err)
		}

		restarted := &Config{}
		if err := restarted.Read(file, nil); err != nil {
			t.Fatalf("read saved config failed: %v", err)
		}
		if restarted.MCPEnabled() || restarted.EnableMCP {
			t.Error("MCP is enabled again after a restart")
		}
		if restarted.JWTIPChangeAllowed() {
			t.Error("JWT IP binding is disabled again after a restart")
		}
		if len(restarted.NATMemberDomains) != 0 {
			t.Errorf("revoked member NAT domains are back: %v", restarted.NATMemberDomains)
		}
		if restarted.ReservedHosts != "" {
			t.Errorf("cleared reserved hosts are back: %q", restarted.ReservedHosts)
		}

		saved, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read saved config: %v", err)
		}
		if strings.Contains(string(saved), "secret-from-environment") {
			t.Errorf("environment JWT secret was written to disk: %s", saved)
		}
		// Settings without a variable keep being omitted while empty.
		if strings.Contains(string(saved), "install_host") {
			t.Errorf("empty setting without an NZ_* variable was written: %s", saved)
		}
	})

	t.Run("first start keeps MCP enabled by the environment", func(t *testing.T) {
		// An empty agent secret makes Read generate one and save the file.
		t.Setenv("NZ_AGENTSECRETKEY", "")
		file := newTempConfig(t, "")
		t.Cleanup(func() { os.Remove(file) })

		for range 2 {
			c := &Config{}
			if err := c.Read(file, nil); err != nil {
				t.Fatalf("read config failed: %v", err)
			}
			if !c.MCPEnabled() {
				t.Fatal("NZ_ENABLEMCP=true did not enable MCP")
			}
		}
	})
}

func newTempConfig(t *testing.T, cfg string) string {
	t.Helper()

	file, err := os.CreateTemp(os.TempDir(), "nezha-test-config-*.yml")
	if err != nil {
		t.Fatalf("create temp file failed: %v", err)
	}
	defer file.Close()

	_, err = file.ReadFrom(strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("write to temp file failed: %v", err)
	}

	return file.Name()
}
