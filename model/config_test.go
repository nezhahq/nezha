package model

import (
	"os"
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
		}

		for _, field := range testFields {
			if !field.Cond {
				t.Fatalf("%s did not passed check, value: %v", field.Name, field.Value)
			}
		}

		os.Remove(file)
	})

	t.Run("ReadFile", func(t *testing.T) {
		const testCfg = "jwt_secret_key: test\nuser_template: um\nadmin_template: am\nagent_secret_key: none\nsite_name: lowkick"

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
		os.Setenv("NZ_JWTSECRETKEY", "test1")
		os.Setenv("NZ_USERTEMPLATE", "um1")
		os.Setenv("NZ_ADMINTEMPLATE", "am1")
		os.Setenv("NZ_AGENTSECRETKEY", "none1")
		os.Setenv("NZ_SITENAME", "lowkick1")

		const testCfg = "jwt_secret_key: test\nuser_template: um\nadmin_template: am\nagent_secret_key: none\nsite_name: lowkick"

		var testFrontendTemplates = []FrontendTemplate{
			{Path: "um"},
			{Path: "am", IsAdmin: true},
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
			{"jwt_secret_key", c.JWTSecretKey, c.JWTSecretKey == "test"},
			{"user_template", c.UserTemplate, c.UserTemplate == "um"},
			{"admin_template", c.AdminTemplate, c.AdminTemplate == "am"},
			{"agent_secret_key", c.AgentSecretKey, c.AgentSecretKey == "none"},
			{"site_name", c.SiteName, c.SiteName == "lowkick"},
		}

		for _, field := range testFields {
			if !field.Cond {
				t.Fatalf("%s did not passed check, value: %v", field.Name, field.Value)
			}
		}

		os.Remove(file)
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
