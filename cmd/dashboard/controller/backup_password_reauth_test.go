package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// Whoever sets or resets the backup password can decrypt every later backup,
// so both need the login password, like applying a restore: a stolen session
// alone must not be able to export the database and config.yaml.
func TestBackupPasswordSetAndResetRequireLoginPassword(t *testing.T) {
	env := newBackupTestEnv(t)
	rec := env.as(env.adminCaller(), "PATCH", "/api/v1/backup/config", map[string]any{"s3": map[string]any{
		"endpoint": "https://s3.example.com", "bucket": "nezha-test", "access_key_id": mockBackupAccessKey, "secret_access_key": mockBackupSecretKey,
	}})
	require.True(t, decodeResponse[any](t, rec).Success, rec.Body.String())

	resp := decodeResponse[BackupKeyResponse](t, env.as(env.adminCaller(), "POST", "/api/v1/backup/password", map[string]any{"password": mockBackupPassword}))
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Error, "login password")
	resp = decodeResponse[BackupKeyResponse](t, env.as(env.adminCaller(), "POST", "/api/v1/backup/password", map[string]any{"password": mockBackupPassword, "login_password": "wrong login password"}))
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Error, "wrong login password")
	assert.Empty(t, resp.Data.RecoveryKey)
	resp = decodeResponse[BackupKeyResponse](t, env.as(env.adminCaller(), "POST", "/api/v1/backup/password", map[string]any{"password": mockBackupPassword, "login_password": mockBackupLoginPassword}))
	require.True(t, resp.Success, resp.Error)
	keyID := resp.Data.KeyID

	reset := map[string]any{"new_password": "another backup password", "confirm": "RESET"}
	resp = decodeResponse[BackupKeyResponse](t, env.as(env.adminCaller(), "POST", "/api/v1/backup/password/reset", reset))
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Error, "login password")
	reset["login_password"] = mockBackupLoginPassword
	resp = decodeResponse[BackupKeyResponse](t, env.as(env.adminCaller(), "POST", "/api/v1/backup/password/reset", reset))
	require.True(t, resp.Success, resp.Error)
	assert.NotEqual(t, keyID, resp.Data.KeyID)
}

// An administrator without a usable password (OAuth only) must have signed
// in recently to set the backup password.
func TestBackupPasswordReauthForOAuthOnlyAdmin(t *testing.T) {
	env := newBackupTestEnv(t)
	require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", 1).Update("reject_password", true).Error)
	require.NoError(t, singleton.DB.Model(&model.JWTSession{}).Where("user_id = ?", 1).Update("created_at", time.Now().Add(-time.Hour)).Error)
	rec := env.as(env.adminCaller(), "PATCH", "/api/v1/backup/config", map[string]any{"s3": map[string]any{
		"endpoint": "https://s3.example.com", "bucket": "nezha-test", "access_key_id": mockBackupAccessKey, "secret_access_key": mockBackupSecretKey,
	}})
	require.True(t, decodeResponse[any](t, rec).Success, rec.Body.String())

	resp := decodeResponse[BackupKeyResponse](t, env.as(env.adminCaller(), "POST", "/api/v1/backup/password", map[string]any{"password": mockBackupPassword}))
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Error, "sign in again")

	require.NoError(t, singleton.DB.Model(&model.JWTSession{}).Where("user_id = ?", 1).Update("created_at", time.Now()).Error)
	resp = decodeResponse[BackupKeyResponse](t, env.as(env.adminCaller(), "POST", "/api/v1/backup/password", map[string]any{"password": mockBackupPassword}))
	assert.True(t, resp.Success, resp.Error)
}
