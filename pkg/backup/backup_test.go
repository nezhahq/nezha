package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyMaterialPasswordLifecycle(t *testing.T) {
	_, _, err := NewKeyMaterial("short", time.Now())
	require.ErrorIs(t, err, ErrPasswordTooShort)

	k, recovery, err := NewKeyMaterial(testPassword, time.Now())
	require.NoError(t, err)
	assert.NotContains(t, k.WrappedIdentity, recovery, "the private key must never be stored in the clear")

	id, err := UnwrapIdentity(k.WrappedIdentity, testPassword)
	require.NoError(t, err)
	assert.Equal(t, k.KeyID, identityKeyID(id))
	_, err = UnwrapIdentity(k.WrappedIdentity, "wrong password!!")
	require.ErrorIs(t, err, ErrWrongPassword)

	rid, err := ParseRecoveryKey("  " + strings.ToLower(recovery) + "\n")
	require.NoError(t, err)
	assert.Equal(t, k.KeyID, identityKeyID(rid))
	_, err = ParseRecoveryKey("AGE-SECRET-KEY-1NOTAKEY")
	require.ErrorIs(t, err, ErrInvalidRecoveryKey)

	changed, err := k.Rewrap(testPassword, "another long password")
	require.NoError(t, err)
	assert.Equal(t, k.KeyID, changed.KeyID, "changing the password keeps the key pair")
	_, err = UnwrapIdentity(changed.WrappedIdentity, "another long password")
	require.NoError(t, err)
	_, err = UnwrapIdentity(changed.WrappedIdentity, testPassword)
	require.ErrorIs(t, err, ErrWrongPassword)
	_, err = k.Rewrap("wrong password!!", "another long password")
	require.ErrorIs(t, err, ErrWrongPassword)
}

func TestNames(t *testing.T) {
	n, err := NewName(time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 8*3600)), TriggerSchedule)
	require.NoError(t, err)
	created, trigger, ok := ParseName(n)
	require.True(t, ok)
	assert.Equal(t, TriggerSchedule, trigger)
	assert.True(t, created.Equal(time.Date(2026, 1, 1, 19, 4, 5, 0, time.UTC)))
	assert.Len(t, ConfirmWord(n), ConfirmWordLength)
	for _, bad := range []string{"", "../nezha-backup-20260101-000000-manual-abcdef", "nezha-backup-20260101-000000-manual-abcdef/x",
		"nezha-backup-20260101-000000-other-abcdef", "nezha-backup-20260101-000000-manual-ABCDEF", "nezha-backup-20260101-000000-manual-abcdef.nzbak"} {
		assert.False(t, ValidName(bad), bad)
	}
}

// The produced file has a plaintext header and an encrypted payload that
// contains the database and the config, matching the manifest.
func TestBackupUploadsEncryptedFile(t *testing.T) {
	e := newTestEnv(t)
	recovery := e.configure()
	b := e.backup(TriggerManual)
	assert.Equal(t, StatusSuccess, b.Status)
	assert.True(t, b.KeyMatchesCurrent)

	keys := e.store.keys()
	require.Equal(t, []string{DefaultPrefix + b.Name + FileExt}, keys)
	data := e.store.objects[keys[0]].data
	sum := sha256.Sum256(data)
	assert.Equal(t, hex.EncodeToString(sum[:]), b.SHA256, "the listed checksum is the checksum of the whole file")
	assert.NotContains(t, string(data), "agent_secret_key", "config must not be readable without the key")
	assert.NotContains(t, string(data), "alpha")

	r := bytes.NewReader(data)
	h, err := ReadHeader(r)
	require.NoError(t, err)
	assert.Equal(t, b.Name, h.Name)
	id, err := ParseRecoveryKey(recovery)
	require.NoError(t, err)
	payload, err := decryptPayload(r, id)
	require.NoError(t, err)
	out := t.TempDir()
	ex, err := extractPayload(payload, out, DefaultMaxBackupBytes)
	require.NoError(t, err)
	assert.True(t, ex.Manifest.matchesHeader(h))
	assert.Equal(t, int64(2), ex.Manifest.Tables["servers"])
	conf, err := os.ReadFile(ex.Paths[FileConfig])
	require.NoError(t, err)
	assert.Contains(t, string(conf), "agent_secret_key: original")

	entries, err := os.ReadDir(filepath.Join(e.dir, tmpDirName))
	if err == nil {
		assert.Empty(t, entries, "nothing may stay in the local temp directory after the upload")
	}
}

func TestBackupRequiresConfiguration(t *testing.T) {
	e := newTestEnv(t)
	_, err := e.m.StartBackup(TriggerManual, admin)
	require.ErrorIs(t, err, ErrNotConfigured)
	_, err = e.m.UpdateConfig(ConfigUpdate{S3: &S3Update{Endpoint: str("https://s3.example.com"), Bucket: str("nezha-test"), AccessKeyID: str(mockAccessKeyID), SecretAccessKey: str(mockSecretAccessKey)}}, admin)
	require.NoError(t, err)
	_, err = e.m.StartBackup(TriggerManual, admin)
	require.ErrorIs(t, err, ErrNotConfigured, "a password is required as well")
}

func TestUploadFailureLeavesNoLocalFile(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	e.store.failPuts = uploadRetries
	job, err := e.m.StartBackup(TriggerManual, admin)
	require.NoError(t, err)
	e.m.op.Lock()
	e.m.op.Unlock()
	list, err := e.m.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, job.Name, list[0].Name)
	assert.Equal(t, StatusFailed, list[0].Status)
	assert.Contains(t, list[0].Error, "injected upload failure")
	assert.Equal(t, uploadRetries, e.store.puts)
	entries, _ := os.ReadDir(filepath.Join(e.dir, tmpDirName))
	assert.Empty(t, entries)

	e.store.failPuts = uploadRetries - 1
	b := e.backup(TriggerManual)
	assert.Equal(t, StatusSuccess, b.Status, "an upload succeeds after transient failures")
}

func TestConfigNeverEchoesSecrets(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	v := e.m.View()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	assert.NotContains(t, string(data), mockSecretAccessKey)
	assert.NotContains(t, string(data), mockAccessKeyID)
	assert.True(t, v.S3.SecretAccessKeySet)
	assert.Equal(t, "****ests", v.S3.AccessKeyIDMasked)
	assert.NotContains(t, string(data), "wrapped_identity")
	assert.NotContains(t, string(data), "auth_key")
	assert.True(t, v.Configured)

	// The settings file holds the secret, but only with owner permissions.
	st, err := os.Stat(filepath.Join(e.dir, SettingsFileName))
	require.NoError(t, err)
	if os.PathSeparator == '/' {
		assert.Equal(t, os.FileMode(0600), st.Mode().Perm())
	}
}

func TestMovingStorageTargetRequiresSecret(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	_, err := e.m.UpdateConfig(ConfigUpdate{S3: &S3Update{Endpoint: str("https://attacker.example.net")}}, admin)
	require.ErrorIs(t, err, ErrSecretRequired)
	_, err = e.m.UpdateConfig(ConfigUpdate{S3: &S3Update{Bucket: str("other-bucket")}}, admin)
	require.ErrorIs(t, err, ErrSecretRequired)
	err = e.m.TestConnection(context.Background(), &S3Update{Endpoint: str("https://attacker.example.net")}, admin)
	require.ErrorIs(t, err, ErrSecretRequired, "the connection test must not send saved credentials elsewhere")

	// Unchanged target, empty secret: the saved secret is kept.
	_, err = e.m.UpdateConfig(ConfigUpdate{S3: &S3Update{Endpoint: str("https://s3.example.com"), SecretAccessKey: str(""), Prefix: str("panel-a")}}, admin)
	require.NoError(t, err)
	v := e.m.View()
	assert.True(t, v.S3.SecretAccessKeySet)
	assert.Equal(t, "panel-a/", v.S3.Prefix)
	require.NoError(t, e.m.TestConnection(context.Background(), nil, admin))

	_, err = e.m.UpdateConfig(ConfigUpdate{S3: &S3Update{Endpoint: str("https://other.example.com"), SecretAccessKey: str("fake-secret-key-for-tests-2")}}, admin)
	require.NoError(t, err)

	for _, bad := range []string{"http://s3.example.com", "https://user:pw@s3.example.com", "https://s3.example.com/path", "ftp://x"} {
		_, err = e.m.UpdateConfig(ConfigUpdate{S3: &S3Update{Endpoint: str(bad), SecretAccessKey: str(mockSecretAccessKey)}}, admin)
		assert.Error(t, err, bad)
	}
	_, err = e.m.UpdateConfig(ConfigUpdate{S3: &S3Update{Endpoint: str("http://127.0.0.1:9000"), SecretAccessKey: str(mockSecretAccessKey)}}, admin)
	assert.Error(t, err, "production accepts https only, also for loopback addresses")
	e.m.opts.AllowLoopbackHTTP = true // integration tests only
	_, err = e.m.UpdateConfig(ConfigUpdate{S3: &S3Update{Endpoint: str("http://127.0.0.1:9000"), SecretAccessKey: str(mockSecretAccessKey)}}, admin)
	assert.NoError(t, err)
	_, err = e.m.UpdateConfig(ConfigUpdate{S3: &S3Update{Prefix: str("../x")}}, admin)
	assert.Error(t, err)
}

func TestEnvironmentCredentialsLockTarget(t *testing.T) {
	t.Setenv(EnvS3SecretAccessKey, "fake-secret-from-env-for-tests")
	e := newTestEnv(t)
	_, err := e.m.UpdateConfig(ConfigUpdate{S3: &S3Update{Endpoint: str("https://s3.example.com")}}, admin)
	require.ErrorIs(t, err, errLockedByEnv, "with an environment secret the target cannot be changed from the dashboard")
	v := e.m.View()
	assert.Contains(t, v.S3.EnvLocked, "secret_access_key")
	assert.Contains(t, v.S3.EnvLocked, "endpoint")
	assert.True(t, v.S3.SecretAccessKeySet)
	data, _ := json.Marshal(v)
	assert.NotContains(t, string(data), "fake-secret-from-env")
}

func TestScheduleValidationAndNextRun(t *testing.T) {
	e := newTestEnv(t)
	_, err := e.m.UpdateConfig(ConfigUpdate{Schedule: &ScheduleUpdate{Cron: str("not a cron")}}, admin)
	require.Error(t, err)
	zero := 0
	_, err = e.m.UpdateConfig(ConfigUpdate{Schedule: &ScheduleUpdate{Retention: &zero}}, admin)
	require.Error(t, err)
	enabled, keep := true, 3
	v, err := e.m.UpdateConfig(ConfigUpdate{Schedule: &ScheduleUpdate{Enabled: &enabled, Cron: str("0 0 4 * * *"), Retention: &keep}}, admin)
	require.NoError(t, err)
	require.NotNil(t, v.Schedule.NextRunAt)
	assert.Equal(t, 4, v.Schedule.NextRunAt.In(time.Local).Hour())

	// Persisted: a new manager reads the same schedule.
	m2 := e.newManager("v1.2.3")
	assert.Equal(t, 3, m2.View().Schedule.Retention)
	assert.True(t, m2.View().Schedule.Enabled)
}

func TestRetentionOnlyDeletesScheduledBackups(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	keep := 2
	_, err := e.m.UpdateConfig(ConfigUpdate{Schedule: &ScheduleUpdate{Retention: &keep}}, admin)
	require.NoError(t, err)
	manual := e.backup(TriggerManual)
	var scheduled []BackupInfo
	for range 4 {
		scheduled = append(scheduled, e.backup(TriggerSchedule))
	}
	keys := e.store.keys()
	assert.Len(t, keys, 3)
	assert.Contains(t, keys, DefaultPrefix+manual.Name+FileExt)
	assert.Contains(t, keys, DefaultPrefix+scheduled[3].Name+FileExt)
	assert.Contains(t, keys, DefaultPrefix+scheduled[2].Name+FileExt)
	assert.NotContains(t, keys, DefaultPrefix+scheduled[0].Name+FileExt)
}

func TestListIgnoresForeignObjectsAndDeleteValidatesNames(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	e.store.objects[DefaultPrefix+"notes.txt"] = memObject{data: []byte("x")}
	e.store.objects["other/"+b.Name+FileExt] = memObject{data: []byte("x")}
	list, err := e.m.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.ErrorIs(t, e.m.Delete(context.Background(), []string{"../../etc/passwd"}, admin), ErrInvalidName)
	require.ErrorIs(t, e.m.Delete(context.Background(), []string{b.Name + FileExt}, admin), ErrInvalidName)
	require.NoError(t, e.m.Delete(context.Background(), []string{b.Name}, admin))
	assert.NotContains(t, e.store.keys(), DefaultPrefix+b.Name+FileExt)
	_, _, err = e.m.Open(context.Background(), "../x", admin)
	require.ErrorIs(t, err, ErrInvalidName)
}

// Writes go on while VACUUM INTO runs; the snapshot is still consistent.
func TestSnapshotUnderConcurrentWrites(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Steady writes, like a dashboard under load; a writer that never
		// pauses would starve any reader past the busy timeout.
		for i := 0; ctx.Err() == nil; i++ {
			_, _ = e.db.Exec("INSERT INTO servers (name) VALUES (?)", strings.Repeat("x", i%100))
			time.Sleep(time.Millisecond)
		}
	}()
	for range 3 {
		b := e.backup(TriggerManual)
		require.Equal(t, StatusSuccess, b.Status, b.Error)
	}
	cancel()
	wg.Wait()
}

// ---- restore ----

func (e *testEnv) prepare(b BackupInfo, password string) (*PrepareResult, error) {
	return e.m.Prepare(context.Background(), PrepareRequest{Name: b.Name, SHA256: b.SHA256, Password: password, Actor: admin})
}

func (e *testEnv) apply(res *PrepareResult, password string) error {
	var acks []string
	for _, w := range res.Warnings {
		if w.RequireAck {
			acks = append(acks, w.Code)
		}
	}
	return e.m.Apply(context.Background(), ApplyRequest{Token: res.RestoreToken, Password: password, Confirm: ConfirmWord(res.Name), Acknowledged: acks, Reauth: okReauth, Actor: admin})
}

func warningCodes(res *PrepareResult) []string {
	var out []string
	for _, w := range res.Warnings {
		out = append(out, w.Code)
	}
	return out
}

func TestPrepareChecksAndStages(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)

	res, err := e.prepare(b, testPassword)
	require.NoError(t, err)
	require.True(t, res.CanApply, "%+v", res.Checks)
	assert.NotEmpty(t, res.RestoreToken)
	for _, c := range res.Checks {
		assert.True(t, c.OK, c.Code)
	}
	assert.Equal(t, int64(2), res.Manifest.Tables["servers"])
	assert.True(t, res.Manifest.HasConfig)
	assert.NotContains(t, warningCodes(res), WarnOriginUnverified, "a backup made by this dashboard is verified")
	assert.Contains(t, warningCodes(res), WarnSessionsCleared)
	assert.Contains(t, warningCodes(res), WarnRevokedCredentials)

	// The staged database no longer has login sessions.
	insp, err := inspectDatabase(filepath.Join(e.m.stage.dir, stageFilesDir, FileDatabase))
	require.NoError(t, err)
	assert.Equal(t, int64(0), insp.tables["jwt_sessions"])
	assert.Equal(t, int64(2), insp.tables["servers"])
}

func TestPrepareRejectsWrongSelection(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	other := e.backup(TriggerManual)

	_, err := e.m.Prepare(context.Background(), PrepareRequest{Name: b.Name, SHA256: other.SHA256, Password: testPassword, Actor: admin})
	require.ErrorIs(t, err, ErrChecksumMismatch, "the checksum binds the request to one backup")
	_, err = e.m.Prepare(context.Background(), PrepareRequest{Name: "../" + b.Name, SHA256: b.SHA256, Password: testPassword, Actor: admin})
	require.ErrorIs(t, err, ErrInvalidName)
	_, err = e.prepare(b, "wrong password!!")
	require.ErrorIs(t, err, ErrWrongPassword)
	_, err = e.m.Prepare(context.Background(), PrepareRequest{Name: b.Name, SHA256: b.SHA256, Actor: admin})
	require.ErrorIs(t, err, errNoCredential)
	assert.Nil(t, e.m.stage)
	entries, _ := os.ReadDir(filepath.Join(e.dir, pendingDirName))
	assert.Empty(t, entries, "failed checks leave nothing staged")
}

func TestRestoreRateLimit(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	for range rateAttempts {
		_, err := e.prepare(b, "wrong password!!")
		require.ErrorIs(t, err, ErrWrongPassword)
	}
	_, err := e.prepare(b, testPassword)
	require.ErrorIs(t, err, ErrRateLimited)
	e.advance(rateWindow)
	_, err = e.prepare(b, testPassword)
	require.NoError(t, err)
}

func TestApplyBindsTokenToSessionAndFiles(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	res, err := e.prepare(b, testPassword)
	require.NoError(t, err)

	base := ApplyRequest{Token: res.RestoreToken, Password: testPassword, Confirm: ConfirmWord(b.Name), Reauth: okReauth, Actor: admin}
	other := base
	other.Actor = Actor{UserID: 2, SessionID: "session-1"}
	require.ErrorIs(t, e.m.Apply(context.Background(), other), ErrRestoreToken, "another user cannot use the token")
	other = base
	other.Actor = Actor{UserID: 1, SessionID: "session-2"}
	require.ErrorIs(t, e.m.Apply(context.Background(), other), ErrRestoreToken, "another session of the same user cannot use the token")
	other = base
	other.Token = strings.Repeat("0", 64)
	require.ErrorIs(t, e.m.Apply(context.Background(), other), ErrRestoreToken)

	other = base
	other.Confirm = "nope"
	require.ErrorIs(t, e.m.Apply(context.Background(), other), ErrConfirmWord)
	other = base
	other.Password = "wrong password!!"
	require.ErrorIs(t, e.m.Apply(context.Background(), other), ErrWrongPassword)
	require.NotNil(t, e.m.stage, "the token survives a few typos")
	e.advance(rateWindow) // stay below the attempt limit; the token lives longer in this test
	e.m.stage.expires = e.m.stage.expires.Add(rateWindow)
	other = base
	other.Reauth = func() error { return errLockedByEnv }
	require.ErrorIs(t, e.m.Apply(context.Background(), other), errLockedByEnv)
	require.Nil(t, e.m.stage, "the third failure discards the staged restore")
	require.ErrorIs(t, e.m.Apply(context.Background(), base), ErrRestoreToken)
}

func TestApplyRejectsExpiredToken(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	res, err := e.prepare(b, testPassword)
	require.NoError(t, err)
	e.advance(RestoreTokenTTL)
	require.ErrorIs(t, e.apply(res, testPassword), ErrRestoreToken)
}

// The object in storage is replaced after the check: Apply uses the staged,
// verified copy and never fetches again; a modified staged file is refused.
func TestApplyUsesVerifiedStagedFiles(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	res, err := e.prepare(b, testPassword)
	require.NoError(t, err)
	e.store.set(DefaultPrefix+b.Name+FileExt, []byte("replaced"))

	staged := filepath.Join(e.m.stage.dir, stageFilesDir, FileConfig)
	require.NoError(t, os.WriteFile(staged, []byte("listen_port: 1\n"), 0600))
	require.ErrorIs(t, e.apply(res, testPassword), ErrCorrupt)
	assert.Nil(t, e.m.stage)
	assert.Zero(t, e.restarts.Load())
}

func TestApplyTakesPreRestoreBackupAndRestarts(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	res, err := e.prepare(b, testPassword)
	require.NoError(t, err)
	require.NoError(t, e.apply(res, testPassword))

	var pre int
	for _, k := range e.store.keys() {
		if strings.Contains(k, "-pre-restore-") {
			pre++
		}
	}
	assert.Equal(t, 1, pre, "the current data is backed up before it is replaced")
	_, err = os.Stat(filepath.Join(e.dir, pendingDirName, pendingFileName))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return e.restarts.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
	_, err = e.m.StartBackup(TriggerManual, admin)
	require.ErrorIs(t, err, ErrRestarting)
}

func TestRestoreFromAnotherDashboardNeedsAcknowledgement(t *testing.T) {
	src := newTestEnv(t)
	src.configure()
	b := src.backup(TriggerManual)
	data := src.store.objects[DefaultPrefix+b.Name+FileExt].data

	dst := newTestEnv(t) // a fresh dashboard: no storage, no password
	up, err := dst.m.UploadTempFile()
	require.NoError(t, err)
	_, err = up.Write(data)
	require.NoError(t, err)
	require.NoError(t, up.Close())
	res, err := dst.m.Prepare(context.Background(), PrepareRequest{UploadPath: up.Name(), Password: testPassword, Actor: admin})
	require.NoError(t, err)
	require.True(t, res.CanApply)
	assert.Equal(t, b.SHA256, res.SHA256)
	assert.Contains(t, warningCodes(res), WarnOriginUnverified)
	assert.Contains(t, warningCodes(res), WarnPreRestoreSkipped)
	_, err = os.Stat(up.Name())
	assert.True(t, os.IsNotExist(err), "the upload is consumed")

	err = dst.m.Apply(context.Background(), ApplyRequest{Token: res.RestoreToken, Password: testPassword, Confirm: ConfirmWord(res.Name), Reauth: okReauth, Actor: admin})
	require.ErrorIs(t, err, ErrAckRequired)
	require.NoError(t, dst.apply(res, testPassword))
}

func TestPasswordChangeAndReset(t *testing.T) {
	e := newTestEnv(t)
	recovery := e.configure()
	old := e.backup(TriggerManual)
	_, err := e.m.ChangePassword(testPassword, "brand new password", admin)
	require.NoError(t, err)

	_, err = e.prepare(old, "brand new password")
	require.NoError(t, err, "on this dashboard the new password opens older backups of the same key")
	_, err = e.prepare(old, testPassword)
	require.NoError(t, err, "the old password still opens the backup itself")

	_, _, err = e.m.ResetPassword("reset password one", "reset", admin)
	require.ErrorIs(t, err, ErrResetConfirm)
	_, _, err = e.m.ResetPassword("reset password one", ResetConfirmWord, admin)
	require.NoError(t, err)
	e.advance(rateWindow)
	_, err = e.prepare(old, "reset password one")
	require.ErrorIs(t, err, ErrWrongPassword, "after a reset, old backups need the old password")
	res, err := e.m.Prepare(context.Background(), PrepareRequest{Name: old.Name, SHA256: old.SHA256, RecoveryKey: recovery, Actor: admin})
	require.NoError(t, err, "or the old recovery key")
	require.True(t, res.CanApply)
	list, err := e.m.List(context.Background())
	require.NoError(t, err)
	assert.False(t, list[0].KeyMatchesCurrent)
}

func TestNewerBackupVersionIsRejected(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	older := e.newManager("v1.0.0")
	res, err := older.Prepare(context.Background(), PrepareRequest{Name: b.Name, SHA256: b.SHA256, Password: testPassword, Actor: admin})
	require.NoError(t, err)
	assert.False(t, res.CanApply)
	assert.Empty(t, res.RestoreToken)
	last := res.Checks[len(res.Checks)-1]
	assert.Equal(t, CheckVersion, last.Code)
	assert.False(t, last.OK)
}

// ---- malicious payloads ----

func encryptedFile(t *testing.T, k KeyMaterial, name string, entries func(tw *tar.Writer)) []byte {
	t.Helper()
	created, trigger, _ := ParseName(name)
	var buf bytes.Buffer
	w, err := encryptTo(&buf, Header{Format: 1, Name: name, CreatedAt: created, Trigger: trigger, NezhaVersion: "v1.2.3", KeyID: k.KeyID, WrappedIdentity: k.WrappedIdentity}, k.Recipient)
	require.NoError(t, err)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	entries(tw)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func TestMaliciousPayloadsAreRejected(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	k := *e.m.settings.Encryption
	name := "nezha-backup-20260101-000000-manual-abcdef"
	add := func(tw *tar.Writer, hdr *tar.Header, body string) {
		hdr.Size = int64(len(body))
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, _ = tw.Write([]byte(body))
	}
	cases := map[string]func(tw *tar.Writer){
		"path traversal": func(tw *tar.Writer) { add(tw, &tar.Header{Name: "../sqlite.db"}, "x") },
		"absolute path":  func(tw *tar.Writer) { add(tw, &tar.Header{Name: "/etc/passwd"}, "x") },
		"symlink": func(tw *tar.Writer) {
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: FileDatabase, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}))
		},
		"unknown file": func(tw *tar.Writer) { add(tw, &tar.Header{Name: "evil.sh"}, "x") },
		"duplicate": func(tw *tar.Writer) {
			add(tw, &tar.Header{Name: FileDatabase}, "a")
			add(tw, &tar.Header{Name: FileDatabase}, "b")
		},
		"no manifest": func(tw *tar.Writer) { add(tw, &tar.Header{Name: FileDatabase}, "a") },
	}
	for label, entries := range cases {
		t.Run(label, func(t *testing.T) {
			data := encryptedFile(t, k, name, entries)
			dir := t.TempDir()
			r := bytes.NewReader(data)
			_, err := ReadHeader(r)
			require.NoError(t, err)
			id, err := UnwrapIdentity(k.WrappedIdentity, testPassword)
			require.NoError(t, err)
			payload, err := decryptPayload(r, id)
			require.NoError(t, err)
			_, err = extractPayload(payload, dir, DefaultMaxBackupBytes)
			require.ErrorIs(t, err, ErrCorrupt)
			_, err = os.Stat(filepath.Join(filepath.Dir(dir), "sqlite.db"))
			assert.True(t, os.IsNotExist(err))
		})
	}

	// A flipped ciphertext byte fails authentication during the check.
	b := e.backup(TriggerManual)
	key := DefaultPrefix + b.Name + FileExt
	data := bytes.Clone(e.store.objects[key].data)
	data[len(data)-20] ^= 0xff
	e.store.set(key, data)
	sum := sha256.Sum256(data)
	res, err := e.m.Prepare(context.Background(), PrepareRequest{Name: b.Name, SHA256: hex.EncodeToString(sum[:]), Password: testPassword, Actor: admin})
	require.NoError(t, err)
	assert.False(t, res.CanApply)
}

func TestHeaderLimits(t *testing.T) {
	_, err := ReadHeader(strings.NewReader("garbage"))
	require.ErrorIs(t, err, ErrNotBackup)
	_, err = ReadHeader(io.MultiReader(strings.NewReader(fileMagic), bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff})))
	require.ErrorIs(t, err, ErrBadHeader)
}

// repack decrypts a backup made by e, lets edit change its manifest / MAC
// and encrypts it again to the same key, as an attacker who knows the
// public key could.
func repack(t *testing.T, e *testEnv, b BackupInfo, recovery string, edit func(m *Manifest, mac *string)) []byte {
	t.Helper()
	data := e.store.objects[DefaultPrefix+b.Name+FileExt].data
	r := bytes.NewReader(data)
	h, err := ReadHeader(r)
	require.NoError(t, err)
	id, err := ParseRecoveryKey(recovery)
	require.NoError(t, err)
	payload, err := decryptPayload(r, id)
	require.NoError(t, err)
	ex, err := extractPayload(payload, t.TempDir(), DefaultMaxBackupBytes)
	require.NoError(t, err)
	m := ex.Manifest
	mac := ex.MAC
	edit(&m, &mac)
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	return encryptedFile(t, *e.m.settings.Encryption, h.Name, func(tw *tar.Writer) {
		for _, name := range []string{FileDatabase, FileConfig} {
			content, err := os.ReadFile(ex.Paths[name])
			require.NoError(t, err)
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(content)), Typeflag: tar.TypeReg}))
			_, _ = tw.Write(content)
		}
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: FileManifest, Mode: 0600, Size: int64(len(raw)), Typeflag: tar.TypeReg}))
		_, _ = tw.Write(raw)
		if mac != "" {
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: FileManifestMAC, Mode: 0600, Size: int64(len(mac)), Typeflag: tar.TypeReg}))
			_, _ = tw.Write([]byte(mac))
		}
	})
}

func (e *testEnv) prepareUpload(data []byte) (*PrepareResult, error) {
	e.t.Helper()
	f, err := e.m.UploadTempFile()
	require.NoError(e.t, err)
	_, err = f.Write(data)
	require.NoError(e.t, err)
	require.NoError(e.t, f.Close())
	return e.m.Prepare(context.Background(), PrepareRequest{UploadPath: f.Name(), Password: testPassword, Actor: admin})
}

func lastCheck(res *PrepareResult) Check { return res.Checks[len(res.Checks)-1] }

// A backup that claims to come from this dashboard must authenticate; it
// cannot be accepted by just acknowledging a warning.
func TestOwnBackupWithBadOriginIsRejected(t *testing.T) {
	e := newTestEnv(t)
	recovery := e.configure()
	b := e.backup(TriggerManual)

	cases := map[string]func(m *Manifest, mac *string){
		"wrong MAC": func(m *Manifest, mac *string) {
			m.Tables["servers"] = 99 // tampered content, MAC no longer matches
		},
		"MAC removed": func(m *Manifest, mac *string) { *mac = "" },
		"forged with our public key, no MAC, no auth key id": func(m *Manifest, mac *string) {
			m.AuthKeyID = ""
			*mac = ""
		},
	}
	for label, edit := range cases {
		t.Run(label, func(t *testing.T) {
			e.advance(rateWindow)
			res, err := e.prepareUpload(repack(t, e, b, recovery, edit))
			require.NoError(t, err)
			assert.False(t, res.CanApply)
			assert.Empty(t, res.RestoreToken)
			assert.Equal(t, CheckOrigin, lastCheck(res).Code)
			assert.False(t, lastCheck(res).OK)
		})
	}

	// The genuine file passes with a verified origin.
	e.advance(rateWindow)
	res, err := e.prepare(b, testPassword)
	require.NoError(t, err)
	require.True(t, res.CanApply)
	assert.Contains(t, checkCodes(res), CheckOrigin)
	assert.NotContains(t, warningCodes(res), WarnOriginUnverified)
}

// A backup from another dashboard (whose key this dashboard does not hold)
// can be restored after an explicit acknowledgement, also when this
// dashboard is configured itself.
func TestForeignBackupNeedsAcknowledgementOnConfiguredDashboard(t *testing.T) {
	src := newTestEnv(t)
	src.configure()
	b := src.backup(TriggerManual)
	data := src.store.objects[DefaultPrefix+b.Name+FileExt].data

	dst := newTestEnv(t)
	dst.configure() // own key pair and MAC key
	res, err := dst.prepareUpload(data)
	require.NoError(t, err)
	require.True(t, res.CanApply)
	assert.Contains(t, warningCodes(res), WarnOriginUnverified)
	assert.NotContains(t, warningCodes(res), WarnPreRestoreSkipped)
	for _, w := range res.Warnings {
		if w.Code == WarnOriginUnverified {
			assert.True(t, w.RequireAck)
		}
	}
	err = dst.m.Apply(context.Background(), ApplyRequest{Token: res.RestoreToken, Password: testPassword, Confirm: ConfirmWord(res.Name), Reauth: okReauth, Actor: admin})
	require.ErrorIs(t, err, ErrAckRequired)
	require.NoError(t, dst.apply(res, testPassword))
}

func checkCodes(res *PrepareResult) []string {
	var out []string
	for _, c := range res.Checks {
		out = append(out, c.Code)
	}
	return out
}

// Backup and restore share one size limit on the unpacked content, so a
// backup that compresses well below the limit is not produced if its
// content could not be unpacked on restore.
func TestBackupSizeLimitMatchesRestore(t *testing.T) {
	const limit = 256 << 10
	big := strings.Repeat("compressible ", 4000) // ~52 KiB per row, compresses to almost nothing

	e := &testEnv{}
	*e = *newTestEnv(t)
	e.maxBytes = limit
	e.m = e.newManager("v1.2.3")
	e.configure()
	for range 8 {
		_, err := e.db.Exec("INSERT INTO servers (name) VALUES (?)", big)
		require.NoError(t, err)
	}
	job, err := e.m.StartBackup(TriggerManual, admin)
	require.NoError(t, err)
	e.m.op.Lock()
	e.m.op.Unlock()
	list, err := e.m.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, job.Name, list[0].Name)
	assert.Equal(t, StatusFailed, list[0].Status)
	assert.Contains(t, list[0].Error, "size limit")
	assert.Empty(t, e.store.keys(), "a backup that could not be restored is never uploaded")

	// Under the limit: backed up and restorable.
	_, err = e.db.Exec("DELETE FROM servers WHERE length(name) > 100; VACUUM")
	require.NoError(t, err)
	_, err = e.db.Exec("INSERT INTO servers (name) VALUES (?)", big)
	require.NoError(t, err)
	b := e.backup(TriggerManual)
	require.Equal(t, StatusSuccess, b.Status, b.Error)
	assert.Less(t, b.Size, int64(limit/4), "the file is compressed")
	res, err := e.prepare(b, testPassword)
	require.NoError(t, err)
	assert.True(t, res.CanApply)

	// The extraction limit is enforced on the unpacked size: a manager with
	// a limit between the compressed and the unpacked size refuses it.
	e.advance(rateWindow)
	e.maxBytes = b.Size * 2
	small := e.newManager("v1.2.3")
	res, err = small.Prepare(context.Background(), PrepareRequest{Name: b.Name, SHA256: b.SHA256, Password: testPassword, Actor: admin})
	require.NoError(t, err)
	assert.False(t, res.CanApply)
	assert.Equal(t, CheckPayload, lastCheck(res).Code)
}

func TestSQLiteURI(t *testing.T) {
	assert.Equal(t, "file:///C:/nezha/data/sqlite.db?mode=ro", sqliteURIFromSlash("C:/nezha/data/sqlite.db", "ro"))
	assert.Equal(t, "file:///dashboard/data/sqlite.db?mode=rw", sqliteURIFromSlash("/dashboard/data/sqlite.db", "rw"))
	assert.Equal(t, "file:///data/a%20b%3Fc.db?mode=ro", sqliteURIFromSlash("/data/a b?c.db", "ro"))
}

// Restoring a backup made by an older version is allowed with an
// informational warning; a version that cannot be compared (dev builds)
// gives a warning as well.
func TestOlderAndUnknownBackupVersions(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual) // made by v1.2.3
	for version, warning := range map[string]string{"v1.3.0": WarnOlderVersion, "debug": WarnVersionUnknown} {
		e.advance(rateWindow)
		m := e.newManager(version)
		res, err := m.Prepare(context.Background(), PrepareRequest{Name: b.Name, SHA256: b.SHA256, Password: testPassword, Actor: admin})
		require.NoError(t, err)
		assert.True(t, res.CanApply, version)
		assert.Contains(t, warningCodes(res), warning, version)
	}
}

// A backup written by a future format version is refused before anything
// is decrypted or staged; so is a file without the format magic.
func TestUnknownBackupFormatIsRefused(t *testing.T) {
	e := newTestEnv(t)
	e.configure()
	b := e.backup(TriggerManual)
	data := e.store.objects[DefaultPrefix+b.Name+FileExt].data
	h, err := ReadHeader(bytes.NewReader(data))
	require.NoError(t, err)
	hdrLen := int(data[len(fileMagic)])<<24 | int(data[len(fileMagic)+1])<<16 | int(data[len(fileMagic)+2])<<8 | int(data[len(fileMagic)+3])
	rest := data[len(fileMagic)+4+hdrLen:]

	h.Format = 2
	raw, err := json.Marshal(h)
	require.NoError(t, err)
	var future bytes.Buffer
	future.WriteString(fileMagic)
	future.Write([]byte{byte(len(raw) >> 24), byte(len(raw) >> 16), byte(len(raw) >> 8), byte(len(raw))})
	future.Write(raw)
	future.Write(rest)
	_, err = e.prepareUpload(future.Bytes())
	require.ErrorIs(t, err, ErrUnsupported)

	e.advance(rateWindow)
	_, err = e.prepareUpload(append([]byte("NZBAK2\n"), data[len(fileMagic):]...))
	require.ErrorIs(t, err, ErrNotBackup)
	entries, _ := os.ReadDir(filepath.Join(e.dir, pendingDirName))
	assert.Empty(t, entries, "nothing is staged")
}

// Versions compare by their core X.Y.Z; suffixes such as -selfhost.N and
// -int.<sha> do not order reliably and only ask for an acknowledgement.
func TestVersionCheckByCoreVersion(t *testing.T) {
	type want struct {
		ok   bool
		warn string
		ack  bool
	}
	cases := []struct {
		backup, panel string
		want          want
	}{
		{"2.7.0", "2.7.0", want{true, "", false}},
		{"2.6.0", "2.7.0", want{true, WarnOlderVersion, false}},
		{"2.8.0", "2.7.0", want{false, "", false}},
		{"2.8.0-selfhost.1-int.aaaa", "2.7.0-selfhost.3", want{false, "", false}},
		{"2.7.0-selfhost.1", "2.8.0", want{true, WarnOlderVersion, false}},
		// same core, different builds: allowed after acknowledgement
		{"2.7.0", "2.7.0-selfhost.1", want{true, WarnVersionDiffers, true}},
		{"2.7.0-selfhost.1", "2.7.0", want{true, WarnVersionDiffers, true}},
		{"2.7.0-selfhost.2", "2.7.0-selfhost.1", want{true, WarnVersionDiffers, true}},
		{"2.7.0-selfhost.1-int.6631de9c", "2.7.0-selfhost.1", want{true, WarnVersionDiffers, true}},
		{"2.7.0-selfhost.1-int.6631de9c", "2.7.0-selfhost.1-int.1a2b3c4d", want{true, WarnVersionDiffers, true}},
		{"v2.7.0-selfhost.1", "2.7.0-selfhost.1", want{true, "", false}},
		// builds that differ only in +build metadata are different builds
		{"2.7.0+build-a", "2.7.0+build-b", want{true, WarnVersionDiffers, true}},
		{"2.7.0-selfhost.1+a", "2.7.0-selfhost.1", want{true, WarnVersionDiffers, true}},
		{"2.7.0+build-a", "2.7.0+build-a", want{true, "", false}},
		{"2.8.0+x", "2.7.0+x", want{false, "", false}},
		// development builds cannot be compared
		{"debug", "2.7.0", want{true, WarnVersionUnknown, true}},
		{"2.8.0", "debug", want{true, WarnVersionUnknown, true}},
	}
	for _, c := range cases {
		m := &Manager{opts: Options{Version: c.panel}}
		check, w := m.versionCheck(c.backup)
		got := want{ok: check.OK}
		if w != nil {
			got.warn, got.ack = w.Code, w.RequireAck
		}
		assert.Equal(t, c.want, got, "backup %s on %s", c.backup, c.panel)
	}
}

// A backup taken by one integration build restores on a later build of the
// same release whatever the commit hashes, once the difference is
// acknowledged.
func TestRestoreAcrossBuildsOfTheSameRelease(t *testing.T) {
	e := newTestEnv(t)
	e.m = e.newManager("2.7.0-selfhost.1-int.6631de9c")
	e.configure()
	b := e.backup(TriggerSchedule)
	e.m = e.newManager("2.7.0-selfhost.1-int.1a2b3c4d")
	res, err := e.prepare(b, testPassword)
	require.NoError(t, err)
	require.True(t, res.CanApply)
	assert.Contains(t, warningCodes(res), WarnVersionDiffers)
	err = e.m.Apply(context.Background(), ApplyRequest{Token: res.RestoreToken, Password: testPassword, Confirm: ConfirmWord(res.Name), Reauth: okReauth, Actor: admin})
	require.ErrorIs(t, err, ErrAckRequired, "the different build must be acknowledged")
	require.NoError(t, e.apply(res, testPassword))
}

// backup.yaml written by a newer version (higher format) turns the feature
// off and is left untouched; a file without the field is format 1 and is
// written back with it.
func TestSettingsFormatVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFileName)
	future := []byte("format: 2\nschedule:\n  enabled: true\n  cron: 0 0 3 * * *\n  retention: 7\nnew_field: kept\n")
	require.NoError(t, os.WriteFile(path, future, 0600))
	_, err := NewManager(Options{DataDir: dir, ConfigPath: filepath.Join(dir, "config.yaml")})
	require.ErrorIs(t, err, ErrSettingsFormat)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(future), string(got), "the newer file is not rewritten")

	require.NoError(t, os.WriteFile(path, []byte("schedule:\n  retention: 5\n"), 0600))
	m, err := NewManager(Options{DataDir: dir, ConfigPath: filepath.Join(dir, "config.yaml")})
	require.NoError(t, err)
	defer m.Close()
	assert.Equal(t, 5, m.View().Schedule.Retention)
	keep := 6
	_, err = m.UpdateConfig(ConfigUpdate{Schedule: &ScheduleUpdate{Retention: &keep}}, admin)
	require.NoError(t, err)
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got), "format: 1")
}
