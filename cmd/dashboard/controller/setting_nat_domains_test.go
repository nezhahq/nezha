package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/i18n"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/require"
)

func TestSettingNATMemberDomainsRoundTrip(t *testing.T) {
	oldConf, oldTemplates, oldLocalizer := singleton.Conf, singleton.FrontendTemplates, singleton.Localizer
	t.Cleanup(func() {
		singleton.Conf, singleton.FrontendTemplates, singleton.Localizer = oldConf, oldTemplates, oldLocalizer
	})
	templates := []model.FrontendTemplate{{Path: "user-dist"}}
	singleton.FrontendTemplates = templates
	path := filepath.Join(t.TempDir(), "dashboard-config.yaml")
	conf := &singleton.ConfigClass{Config: &model.Config{}}
	require.NoError(t, conf.Read(path, templates))
	singleton.Conf = conf
	singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)

	patch := func(domains any, include bool) error {
		fields := map[string]any{"user_template": "user-dist", "language": "en-US", "dashboard_host": "panel.example.com"}
		if include {
			fields["nat_member_domains"] = domains
		}
		data, err := json.Marshal(fields)
		require.NoError(t, err)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPatch, "/api/v1/setting", bytes.NewReader(data))
		c.Request.Header.Set("Content-Type", "application/json")
		_, err = updateConfig(c)
		return err
	}
	require.NoError(t, patch([]string{" Tunnel.Example.COM. ", "tunnel.example.com", "files.example.com"}, true))
	expected := []string{"tunnel.example.com", "files.example.com"}
	require.Equal(t, expected, conf.NATMemberDomains, "the settings API must save the member allowlist")
	require.True(t, singleton.NATDomainAllowed("tunnel.example.com", false))
	require.False(t, singleton.NATDomainAllowed("other.example.com", false))
	restored := &model.Config{}
	require.NoError(t, restored.Read(path, templates))
	require.Equal(t, expected, restored.NATMemberDomains)

	admin := ctxAs(1, model.RoleAdmin)
	response, err := listConfig(admin)
	require.NoError(t, err)
	data, err := json.Marshal(response.Config)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(data, &fields))
	require.Equal(t, []any{"tunnel.example.com", "files.example.com"}, fields["nat_member_domains"])
	member := ctxAs(2, model.RoleMember)
	response, err = listConfig(member)
	require.NoError(t, err)
	data, err = json.Marshal(response.Config)
	require.NoError(t, err)
	require.NotContains(t, string(data), "tunnel.example.com", "members must not receive administrator settings")

	require.NoError(t, patch(nil, false))
	require.Equal(t, expected, conf.NATMemberDomains, "older clients must not erase an omitted allowlist")
	for _, invalid := range []string{"*.example.com", "https://tunnel.example.com", "invalid host", "panel.example.com"} {
		require.Error(t, patch([]string{invalid}, true), invalid)
		require.Equal(t, expected, conf.NATMemberDomains, "invalid updates must leave the policy unchanged")
	}
	require.NoError(t, patch([]string{}, true))
	require.Empty(t, conf.NATMemberDomains, "an explicit empty list must disable member NAT")
	require.False(t, singleton.NATDomainAllowed("tunnel.example.com", false))
	restored = &model.Config{}
	require.NoError(t, restored.Read(path, templates))
	require.Empty(t, restored.NATMemberDomains)
}
