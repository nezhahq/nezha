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
	assert.NoError(t, err, "plain http is allowed for a loopback MinIO")
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
		for i := 0; ctx.Err() == nil; i++ {
			_, _ = e.db.Exec("INSERT INTO servers (name) VALUES (?)", strings.Repeat("x", i%100))
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
