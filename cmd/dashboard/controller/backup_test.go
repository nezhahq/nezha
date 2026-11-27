package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/backup"
	"github.com/nezhahq/nezha/pkg/idcodec"
	"github.com/nezhahq/nezha/service/singleton"
)

// mock: fake S3 credentials and passwords used only by these tests.
const (
	mockBackupSecretKey     = "fake-backup-secret-key-for-tests"
	mockBackupAccessKey     = "fake-backup-access-key-for-tests"
	mockBackupPassword      = "backup password for tests"
	mockBackupLoginPassword = "login password for tests"
)

// backupMemStorage is an in-process backup.Storage.
type backupMemStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	meta    map[string]map[string]string
}

func (s *backupMemStorage) Put(_ context.Context, key string, r io.Reader, _ int64, meta map[string]string) (backup.ObjectInfo, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return backup.ObjectInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = data
	s.meta[key] = meta
	return backup.ObjectInfo{Key: key, Size: int64(len(data)), Metadata: meta}, nil
}

func (s *backupMemStorage) Get(_ context.Context, key string) (io.ReadCloser, backup.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, backup.ObjectInfo{}, backup.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), backup.ObjectInfo{Key: key, Size: int64(len(data)), Metadata: s.meta[key]}, nil
}

func (s *backupMemStorage) Stat(ctx context.Context, key string) (backup.ObjectInfo, error) {
	rc, info, err := s.Get(ctx, key)
	if err == nil {
		rc.Close()
	}
	return info, err
}

func (s *backupMemStorage) List(_ context.Context, prefix string) ([]backup.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []backup.ObjectInfo
	for k, v := range s.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, backup.ObjectInfo{Key: k, Size: int64(len(v)), ETag: k})
		}
	}
	return out, nil
}

func (s *backupMemStorage) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

type backupTestEnv struct {
	router   *gin.Engine
	store    *backupMemStorage
	admin    *http.Cookie
	member   *http.Cookie
	csrf     string
	pat      string
	restarts atomic.Int32
	skew     atomic.Int64 // added to the manager's clock
	dir      string
}

func (env *backupTestEnv) now() time.Time { return time.Now().Add(time.Duration(env.skew.Load())) }

func (env *backupTestEnv) managerOptions() backup.Options {
	return backup.Options{
		DataDir:    env.dir,
		ConfigPath: filepath.Join(env.dir, "config.yaml"),
		Version:    "v1.2.3",
		Snapshot: func(ctx context.Context, dst string) error {
			return singleton.DB.WithContext(ctx).Exec("VACUUM INTO ?", dst).Error
		},
		NewStorage: func(backup.S3Settings) (backup.Storage, error) { return env.store, nil },
		Restart:    func() { env.restarts.Add(1) },
		RetryDelay: time.Millisecond,
		Now:        env.now,
	}
}

func newBackupTestEnv(t *testing.T) *backupTestEnv {
	t.Helper()
	dir := t.TempDir()
	cleanup, memberID := setupMCPTestWithDatabase(t, filepath.Join(dir, "sqlite.db"))
	t.Cleanup(cleanup)
	singleton.Conf.JWTSecretKey = strings.Repeat("backup-route-test-signing-key-", 3)
	require.NoError(t, idcodec.Init([]byte(singleton.Conf.JWTSecretKey)))
	require.NoError(t, singleton.DB.AutoMigrate(&model.JWTSession{}))
	hash, err := bcrypt.GenerateFromPassword([]byte(mockBackupLoginPassword), bcrypt.MinCost)
	require.NoError(t, err)
	adminUser := model.User{Common: model.Common{ID: 1}, Username: "root", Password: string(hash), Role: model.RoleAdmin}
	require.NoError(t, singleton.DB.Create(&adminUser).Error)

	backup.ScryptWorkFactor = 10
	env := &backupTestEnv{store: &backupMemStorage{objects: map[string][]byte{}, meta: map[string]map[string]string{}}}
	oldBackup, oldRestart := singleton.BackupShared, singleton.RequestRestart
	t.Cleanup(func() { singleton.BackupShared, singleton.RequestRestart = oldBackup, oldRestart })
	env.dir = dir
	m, err := backup.NewManager(env.managerOptions())
	require.NoError(t, err)
	t.Cleanup(m.Close)
	singleton.BackupShared = m

	gin.SetMode(gin.TestMode)
	env.router = gin.New()
	oldParser := dashboardCredentialJWTParser
	t.Cleanup(func() { dashboardCredentialJWTParser = oldParser })
	routers(env.router, fstest.MapFS{})

	env.admin = env.login(t, &adminUser)
	var member model.User
	require.NoError(t, singleton.DB.First(&member, memberID).Error)
	env.member = env.login(t, &member)
	env.csrf = issueCSRFToken()
	_, env.pat = mkToken(t, adminUser.ID, []string{model.ScopeNezhaAll}, nil)
	return env
}

func (env *backupTestEnv) login(t *testing.T, user *model.User) *http.Cookie {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "https://example.com/api/v1/login", nil)
	claims, err := issueJWTSession(ctx, user, 1)
	require.NoError(t, err)
	mw, err := jwt.New(initParams())
	require.NoError(t, err)
	token, _, err := mw.TokenGenerator(claims)
	require.NoError(t, err)
	return &http.Cookie{Name: "nz-jwt", Value: token}
}

type backupCaller struct {
	cookie *http.Cookie
	csrf   string
	pat    string
}

func (env *backupTestEnv) as(c backupCaller, method, path string, body any) *httptest.ResponseRecorder {
	var reader io.Reader
	contentType := "application/json"
	switch b := body.(type) {
	case nil:
	case *multipartBody:
		reader, contentType = bytes.NewReader(b.data), b.contentType
	default:
		data, _ := json.Marshal(b)
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, "https://example.com"+path, reader)
	req.Header.Set("Content-Type", contentType)
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	if c.csrf != "" {
		req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: c.csrf})
		req.Header.Set(csrfHeaderName, c.csrf)
	}
	if c.pat != "" {
		req.Header.Set("Authorization", "Bearer "+c.pat)
	}
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	return rec
}

func (env *backupTestEnv) adminCaller() backupCaller {
	return backupCaller{cookie: env.admin, csrf: env.csrf}
}

type multipartBody struct {
	data        []byte
	contentType string
}

func newMultipart(t *testing.T, fields map[string]string, file []byte) *multipartBody {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", "backup.nzbak")
	require.NoError(t, err)
	_, err = fw.Write(file)
	require.NoError(t, err)
	for k, v := range fields {
		require.NoError(t, w.WriteField(k, v))
	}
	require.NoError(t, w.Close())
	return &multipartBody{data: buf.Bytes(), contentType: w.FormDataContentType()}
}

func decodeResponse[T any](t *testing.T, rec *httptest.ResponseRecorder) model.CommonResponse[T] {
	t.Helper()
	var resp model.CommonResponse[T]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp
}

func (env *backupTestEnv) configure(t *testing.T) {
	t.Helper()
	rec := env.as(env.adminCaller(), "PATCH", "/api/v1/backup/config", map[string]any{"s3": map[string]any{
		"endpoint": "https://s3.example.com", "bucket": "nezha-test", "access_key_id": mockBackupAccessKey, "secret_access_key": mockBackupSecretKey,
	}})
	require.True(t, decodeResponse[any](t, rec).Success, rec.Body.String())
	rec = env.as(env.adminCaller(), "POST", "/api/v1/backup/password", map[string]any{"password": mockBackupPassword, "login_password": mockBackupLoginPassword})
	resp := decodeResponse[BackupKeyResponse](t, rec)
	require.True(t, resp.Success, rec.Body.String())
	require.True(t, strings.HasPrefix(resp.Data.RecoveryKey, "AGE-SECRET-KEY-1"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func (env *backupTestEnv) createBackup(t *testing.T) backup.BackupInfo {
	t.Helper()
	rec := env.as(env.adminCaller(), "POST", "/api/v1/backup", nil)
	resp := decodeResponse[backup.BackupInfo](t, rec)
	require.True(t, resp.Success, rec.Body.String())
	assert.Equal(t, backup.StatusRunning, resp.Data.Status)
	var found backup.BackupInfo
	require.Eventually(t, func() bool {
		list := decodeResponse[[]backup.BackupInfo](t, env.as(env.adminCaller(), "GET", "/api/v1/backup", nil))
		for _, b := range list.Data {
			if b.Name == resp.Data.Name && b.Status == backup.StatusSuccess {
				found = b
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond)
	return found
}

// Every backup route refuses API tokens (even with full scope), members and
// cookie requests without the CSRF pair.
func TestBackupRoutesAccessControl(t *testing.T) {
	env := newBackupTestEnv(t)
	routes := []struct{ method, path string }{
		{"GET", "/api/v1/backup"},
		{"POST", "/api/v1/backup"},
		{"GET", "/api/v1/backup/config"},
		{"PATCH", "/api/v1/backup/config"},
		{"POST", "/api/v1/backup/config/test"},
		{"POST", "/api/v1/backup/password"},
		{"PATCH", "/api/v1/backup/password"},
		{"POST", "/api/v1/backup/password/reset"},
		{"GET", "/api/v1/backup/nezha-backup-20260101-000000-manual-abcdef/download"},
		{"POST", "/api/v1/batch-delete/backup"},
		{"POST", "/api/v1/backup/restore/prepare"},
		{"POST", "/api/v1/backup/restore/apply"},
		{"DELETE", "/api/v1/backup/restore/abc"},
		{"GET", "/api/v1/backup/restore/status"},
	}
	for _, r := range routes {
		rec := env.as(backupCaller{pat: env.pat}, r.method, r.path, map[string]any{})
		assert.Equal(t, http.StatusForbidden, rec.Code, "PAT %s %s: %s", r.method, r.path, rec.Body.String())

		rec = env.as(backupCaller{cookie: env.member, csrf: env.csrf}, r.method, r.path, map[string]any{})
		resp := decodeResponse[any](t, rec)
		assert.False(t, resp.Success, "member %s %s", r.method, r.path)
		assert.Contains(t, resp.Error, "permission denied", "member %s %s", r.method, r.path)

		rec = env.as(backupCaller{}, r.method, r.path, map[string]any{})
		assert.False(t, strings.Contains(rec.Body.String(), `"success":true`), "anonymous %s %s", r.method, r.path)

		if r.method != "GET" {
			rec = env.as(backupCaller{cookie: env.admin}, r.method, r.path, map[string]any{})
			assert.Equal(t, http.StatusForbidden, rec.Code, "no CSRF %s %s", r.method, r.path)
			assert.Contains(t, rec.Body.String(), "CSRF")
		}
	}
}

func TestBackupConfigNeverReturnsSecrets(t *testing.T) {
	env := newBackupTestEnv(t)
	env.configure(t)
	for _, path := range []string{"/api/v1/backup/config", "/api/v1/setting"} {
		rec := env.as(env.adminCaller(), "GET", path, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.NotContains(t, rec.Body.String(), mockBackupSecretKey, path)
		assert.NotContains(t, rec.Body.String(), mockBackupAccessKey, path)
		assert.NotContains(t, rec.Body.String(), "AGE-SECRET-KEY", path)
	}
	rec := env.as(env.adminCaller(), "PATCH", "/api/v1/backup/config", map[string]any{"s3": map[string]any{"endpoint": "https://attacker.example.net"}})
	resp := decodeResponse[any](t, rec)
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Error, "secret access key")
}

func TestBackupDownloadAndRestoreThroughAPI(t *testing.T) {
	env := newBackupTestEnv(t)
	env.configure(t)
	b := env.createBackup(t)

	rec := env.as(env.adminCaller(), "GET", "/api/v1/backup/"+b.Name+"/download", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `attachment; filename=`+b.Name+`.nzbak`, rec.Header().Get("Content-Disposition"))
	assert.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"))
	sum := sha256.Sum256(rec.Body.Bytes())
	assert.Equal(t, b.SHA256, hex.EncodeToString(sum[:]), "the downloaded file matches the listed checksum")
	file := rec.Body.Bytes()

	rec = env.as(env.adminCaller(), "GET", "/api/v1/backup/..%2F..%2Fetc/download", nil)
	assert.NotContains(t, rec.Body.String(), `"success":true`)

	rec = env.as(env.adminCaller(), "POST", "/api/v1/backup/restore/prepare", map[string]any{"name": b.Name, "sha256": b.SHA256, "password": mockBackupPassword})
	prep := decodeResponse[RestorePrepareResponse](t, rec)
	require.True(t, prep.Success, rec.Body.String())
	require.True(t, prep.Data.CanApply)
	assert.True(t, prep.Data.ApplyRequires.LoginPassword)
	assert.False(t, prep.Data.ApplyRequires.ReauthNeeded)

	apply := map[string]any{"restore_token": prep.Data.RestoreToken, "password": mockBackupPassword, "confirm": backup.ConfirmWord(b.Name)}
	resp := decodeResponse[any](t, env.as(env.adminCaller(), "POST", "/api/v1/backup/restore/apply", apply))
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Error, "login password")
	apply["login_password"] = "wrong login password"
	resp = decodeResponse[any](t, env.as(env.adminCaller(), "POST", "/api/v1/backup/restore/apply", apply))
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Error, "wrong login password")

	// An upload of the downloaded file is checked the same way; it replaces
	// the staged restore of the previous check.
	rec = env.as(env.adminCaller(), "POST", "/api/v1/backup/restore/prepare", newMultipart(t, map[string]string{"password": mockBackupPassword}, file))
	up := decodeResponse[RestorePrepareResponse](t, rec)
	require.True(t, up.Success, rec.Body.String())
	assert.Equal(t, b.SHA256, up.Data.SHA256)
	assert.Equal(t, "upload", up.Data.Source)

	apply = map[string]any{"restore_token": up.Data.RestoreToken, "password": mockBackupPassword, "confirm": backup.ConfirmWord(b.Name), "login_password": mockBackupLoginPassword}
	rec = env.as(env.adminCaller(), "POST", "/api/v1/backup/restore/apply", apply)
	require.True(t, decodeResponse[any](t, rec).Success, rec.Body.String())
	require.Eventually(t, func() bool { return env.restarts.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
}

// The restore upload may exceed the 1 MiB API body limit; other routes may not.
func TestBackupUploadBodyLimit(t *testing.T) {
	env := newBackupTestEnv(t)
	big := bytes.Repeat([]byte("x"), 2<<20)
	rec := env.as(env.adminCaller(), "POST", "/api/v1/backup/restore/prepare", newMultipart(t, map[string]string{"password": mockBackupPassword}, big))
	resp := decodeResponse[any](t, rec)
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Error, "not a nezha backup file", "the upload reached the check instead of hitting the body limit")

	rec = env.as(env.adminCaller(), "PATCH", "/api/v1/backup/config", map[string]any{"s3": map[string]any{"prefix": strings.Repeat("a", 2<<20)}})
	assert.False(t, decodeResponse[any](t, rec).Success)
}

// An administrator without a usable password (OAuth only) must have signed
// in recently to apply a restore.
func TestRestoreReauthForOAuthOnlyAdmin(t *testing.T) {
	env := newBackupTestEnv(t)
	env.configure(t)
	b := env.createBackup(t)
	require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", 1).Update("reject_password", true).Error)
	require.NoError(t, singleton.DB.Model(&model.JWTSession{}).Where("user_id = ?", 1).Update("created_at", time.Now().Add(-time.Hour)).Error)

	rec := env.as(env.adminCaller(), "POST", "/api/v1/backup/restore/prepare", map[string]any{"name": b.Name, "sha256": b.SHA256, "password": mockBackupPassword})
	prep := decodeResponse[RestorePrepareResponse](t, rec)
	require.True(t, prep.Success, rec.Body.String())
	assert.False(t, prep.Data.ApplyRequires.LoginPassword)
	assert.True(t, prep.Data.ApplyRequires.ReauthNeeded)

	apply := map[string]any{"restore_token": prep.Data.RestoreToken, "password": mockBackupPassword, "confirm": backup.ConfirmWord(b.Name), "login_password": mockBackupLoginPassword}
	resp := decodeResponse[any](t, env.as(env.adminCaller(), "POST", "/api/v1/backup/restore/apply", apply))
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Error, "sign in again")

	require.NoError(t, singleton.DB.Model(&model.JWTSession{}).Where("user_id = ?", 1).Update("created_at", time.Now()).Error)
	resp = decodeResponse[any](t, env.as(env.adminCaller(), "POST", "/api/v1/backup/restore/apply", apply))
	assert.True(t, resp.Success, resp.Error)
}

// An apply with a restore token that is invalid, cancelled, expired,
// discarded after failed attempts or already used fails with the
// untranslated ApiErrorRestoreToken prefix, which the admin panel uses to go
// back to the check.
func TestRestoreTokenErrorsCarryTheSignal(t *testing.T) {
	env := newBackupTestEnv(t)
	env.configure(t)
	b := env.createBackup(t)
	prepare := func() string {
		env.skew.Add(int64(16 * time.Minute)) // stay below the attempt limit
		rec := env.as(env.adminCaller(), "POST", "/api/v1/backup/restore/prepare", map[string]any{"name": b.Name, "sha256": b.SHA256, "password": mockBackupPassword})
		resp := decodeResponse[RestorePrepareResponse](t, rec)
		require.True(t, resp.Success, rec.Body.String())
		return resp.Data.RestoreToken
	}
	apply := func(token, confirm string) model.CommonResponse[any] {
		return decodeResponse[any](t, env.as(env.adminCaller(), "POST", "/api/v1/backup/restore/apply", map[string]any{
			"restore_token": token, "password": mockBackupPassword, "confirm": confirm, "login_password": mockBackupLoginPassword}))
	}
	assertSignal := func(label string, resp model.CommonResponse[any]) {
		t.Helper()
		assert.False(t, resp.Success, label)
		assert.True(t, strings.HasPrefix(resp.Error, RestoreTokenErrorCode+": "), "%s: %q", label, resp.Error)
	}
	confirm := backup.ConfirmWord(b.Name)

	prepare()
	assertSignal("unknown token", apply(strings.Repeat("0", 64), confirm))

	token := prepare()
	require.True(t, decodeResponse[any](t, env.as(env.adminCaller(), "DELETE", "/api/v1/backup/restore/"+token, nil)).Success)
	assertSignal("cancelled token", apply(token, confirm))

	token = prepare()
	env.skew.Add(int64(backup.RestoreTokenTTL + time.Second))
	assertSignal("expired token", apply(token, confirm))

	token = prepare()
	for range 3 {
		resp := apply(token, "nope")
		require.False(t, resp.Success)
		require.False(t, strings.HasPrefix(resp.Error, RestoreTokenErrorCode), "a wrong confirmation is not a token error")
	}
	assertSignal("token discarded after failed attempts", apply(token, confirm))

	token = prepare()
	require.True(t, apply(token, confirm).Success)
	// The dashboard restarts; the next process has no staged restore.
	m, err := backup.NewManager(env.managerOptions())
	require.NoError(t, err)
	t.Cleanup(m.Close)
	singleton.BackupShared = m
	assertSignal("used token", apply(token, confirm))
}
