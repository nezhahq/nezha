package controller

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
)

func TestSettingForm_AllowJWTIPChangePreservesAbsentAndDecodesExplicitValues(t *testing.T) {
	var omitted model.SettingForm
	require.NoError(t, json.Unmarshal([]byte(`{"site_name":"X"}`), &omitted))
	require.Nil(t, omitted.AllowJWTIPChange)

	var enabled model.SettingForm
	require.NoError(t, json.Unmarshal([]byte(`{"allow_jwt_ip_change":true}`), &enabled))
	require.NotNil(t, enabled.AllowJWTIPChange)
	require.True(t, *enabled.AllowJWTIPChange)

	var disabled model.SettingForm
	require.NoError(t, json.Unmarshal([]byte(`{"allow_jwt_ip_change":false}`), &disabled))
	require.NotNil(t, disabled.AllowJWTIPChange)
	require.False(t, *disabled.AllowJWTIPChange)
}
