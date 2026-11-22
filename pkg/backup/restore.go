package backup

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"golang.org/x/mod/semver"
)

// opWait is how long a restore check or apply waits for a running backup
// (e.g. one that has just uploaded and is still applying retention).
const opWait = 30 * time.Second

// lockOp takes the operation lock, waiting up to opWait for a running one.
func (m *Manager) lockOp(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, opWait)
	defer cancel()
	for {
		if m.op.TryLock() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// RestoreTokenTTL is how long a prepared restore can be applied.
const RestoreTokenTTL = 10 * time.Minute

// maxApplyFailures is how many failed apply attempts a token survives.
const maxApplyFailures = 3

var (
	ErrRestoreToken     = errors.New("restore token is invalid or expired; run the check again")
	ErrChecksumMismatch = errors.New("the backup does not match the selected checksum")
	ErrConfirmWord      = errors.New("the confirmation text does not match the backup name")
	ErrAckRequired      = errors.New("all warnings that require confirmation must be acknowledged")
	ErrTooLarge         = errors.New("backup file exceeds the size limit")
	errNoCredential     = errors.New("enter the backup password or the recovery key")
)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Check is one verification step of a restore check.
type Check struct {
	Code    string `json:"code"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// Warning is something the administrator must know before applying a
// restore. Warnings with RequireAck must be acknowledged explicitly.
type Warning struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	RequireAck bool   `json:"require_ack"`
}

// Check and warning codes.
const (
	CheckChecksum = "checksum"
	CheckDecrypt  = "decrypt"
	CheckPayload  = "payload"
	CheckVersion  = "version"
	CheckDatabase = "database_integrity"
	CheckAdmin    = "admin_user"
	CheckOrigin   = "origin"

	WarnOriginUnverified    = "origin_unverified"
	WarnPreRestoreSkipped   = "pre_restore_skipped"
	WarnOlderVersion        = "older_version"
	WarnVersionUnknown      = "version_unknown"
	WarnVersionDiffers      = "version_differs"
	WarnTSDBNotRestored     = "tsdb_not_restored"
	WarnAgentsAfterBackup   = "agents_after_backup_offline"
	WarnRevokedCredentials  = "revoked_credentials_revived" // #nosec G101 -- warning code returned to the client, not a credential
	WarnSessionsCleared     = "sessions_cleared"
	WarnConfigNotInBackup   = "config_not_in_backup"
	sourceStorage           = "storage"
	sourceUpload            = "upload"
	stageFilesDir           = "files"
	stageBackupFile         = "backup" + FileExt
	tokenBytes              = 32
	stageIDBytes            = 8
	defaultApplyRestartWait = time.Second
)

// ManifestView is the part of the encrypted manifest shown after a check.
type ManifestView struct {
	Name         string           `json:"name"`
	CreatedAt    time.Time        `json:"created_at"`
	Trigger      Trigger          `json:"trigger"`
	NezhaVersion string           `json:"nezha_version"`
	KeyID        string           `json:"key_id"`
	Tables       map[string]int64 `json:"tables"`
	HasConfig    bool             `json:"has_config"`
}

// PrepareRequest asks to check a backup and stage it for restore. Exactly one
// of (Name, SHA256) and UploadPath selects the backup.
type PrepareRequest struct {
	Name        string
	SHA256      string
	UploadPath  string
	Password    string
	RecoveryKey string
	Actor       Actor
}

// PrepareResult is the outcome of a restore check. RestoreToken is set only
// when every check passed.
type PrepareResult struct {
	RestoreToken      string        `json:"restore_token,omitempty"`
	ExpiresAt         *time.Time    `json:"expires_at,omitempty"`
	Source            string        `json:"source"`
	Name              string        `json:"name"`
	SHA256            string        `json:"sha256"`
	ConfirmWordLength int           `json:"confirm_word_length"`
	Manifest          *ManifestView `json:"manifest,omitempty"`
	Checks            []Check       `json:"checks"`
	Warnings          []Warning     `json:"warnings"`
	CanApply          bool          `json:"can_apply"`
}

type restoreStage struct {
	token      string
	userID     uint64
	sessionID  string
	name       string
	sha256     string
	keyID      string
	header     Header
	dir        string
	files      map[string]string // entry name -> sha256 of the staged file
	requireAck []string
	expires    time.Time
	failures   int
}

// UploadTempFile creates a file for an uploaded backup below the data
// directory. The caller writes at most MaxBackupBytes into it.
func (m *Manager) UploadTempFile() (*os.File, error) {
	dir := filepath.Join(m.opts.DataDir, tmpDirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, "upload-*"+FileExt)
}

// MaxBackupBytes is the size limit of backup files.
func (m *Manager) MaxBackupBytes() int64 { return m.opts.MaxBackupBytes }

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Prepare downloads (or takes the uploaded) backup, verifies that it is the
// one the administrator selected, decrypts and checks it, and stages the
// verified files for Apply. A new Prepare replaces any staged restore.
func (m *Manager) Prepare(ctx context.Context, req PrepareRequest) (*PrepareResult, error) {
	if req.UploadPath != "" {
		defer os.Remove(req.UploadPath)
	}
	if err := m.checkRestarting(); err != nil {
		return nil, err
	}
	if err := m.limiter.allow(req.Actor.UserID, "restore"); err != nil {
		return nil, err
	}
	if req.Password == "" && req.RecoveryKey == "" {
		return nil, errNoCredential
	}
	if !m.lockOp(ctx) {
		return nil, ErrBusy
	}
	defer m.op.Unlock()
	m.restoreMu.Lock()
	m.dropStageLocked()
	m.restoreMu.Unlock()

	res, stage, err := m.prepare(ctx, req)
	m.audit.record(auditEntry{Action: "restore_check", Actor: req.Actor, Name: resultName(res, req), SHA256: resultSHA(res), Result: prepareResult(res, err), Error: errString(err)})
	if err != nil || stage == nil {
		return res, err
	}
	m.restoreMu.Lock()
	m.stage = stage
	m.restoreMu.Unlock()
	time.AfterFunc(RestoreTokenTTL+time.Minute, func() { m.expireStage(stage) })
	return res, nil
}

func resultName(res *PrepareResult, req PrepareRequest) string {
	if res != nil && res.Name != "" {
		return res.Name
	}
	if ValidName(req.Name) {
		return req.Name
	}
	return ""
}

func resultSHA(res *PrepareResult) string {
	if res != nil {
		return res.SHA256
	}
	return ""
}

func prepareResult(res *PrepareResult, err error) string {
	if err != nil || res == nil || !res.CanApply {
		return "failed"
	}
	return "ok"
}

func (m *Manager) prepare(ctx context.Context, req PrepareRequest) (*PrepareResult, *restoreStage, error) {
	s, _ := m.effective()
	id, err := randomHex(stageIDBytes)
	if err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(m.opts.DataDir, pendingDirName, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()

	res := &PrepareResult{ConfirmWordLength: ConfirmWordLength, Checks: []Check{}, Warnings: []Warning{}}
	backupPath := filepath.Join(dir, stageBackupFile)
	if req.UploadPath != "" {
		res.Source = sourceUpload
		if err := os.Rename(req.UploadPath, backupPath); err != nil {
			return nil, nil, err
		}
		sum, size, err := fileSHA256(backupPath)
		if err != nil {
			return nil, nil, err
		}
		if size > m.opts.MaxBackupBytes {
			return nil, nil, ErrTooLarge
		}
		res.SHA256 = sum
	} else {
		res.Source = sourceStorage
		want := strings.ToLower(strings.TrimSpace(req.SHA256))
		if !ValidName(req.Name) {
			return nil, nil, ErrInvalidName
		}
		if !sha256Re.MatchString(want) {
			return nil, nil, ErrChecksumMismatch
		}
		sum, err := m.download(ctx, s, req.Name, backupPath)
		if err != nil {
			return nil, nil, err
		}
		if subtle.ConstantTimeCompare([]byte(sum), []byte(want)) != 1 {
			return nil, nil, ErrChecksumMismatch
		}
		res.SHA256 = sum
		res.Name = req.Name
	}
	res.Checks = append(res.Checks, Check{Code: CheckChecksum, OK: true})

	f, err := os.Open(backupPath) // #nosec G304 -- staging file created above
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	header, err := ReadHeader(f)
	if err != nil {
		return nil, nil, err
	}
	if res.Source == sourceStorage && header.Name != req.Name {
		return nil, nil, ErrChecksumMismatch
	}
	res.Name = header.Name
	identity, err := m.unlock(header, req.Password, req.RecoveryKey, s)
	if err != nil {
		return nil, nil, err
	}

	fail := func(code string, err error) (*PrepareResult, *restoreStage, error) {
		res.Checks = append(res.Checks, Check{Code: code, OK: false, Message: err.Error()})
		return res, nil, nil
	}
	payload, err := decryptPayload(f, identity)
	if err != nil {
		return fail(CheckDecrypt, err)
	}
	res.Checks = append(res.Checks, Check{Code: CheckDecrypt, OK: true})
	filesDir := filepath.Join(dir, stageFilesDir)
	if err := os.Mkdir(filesDir, 0700); err != nil {
		return nil, nil, err
	}
	ex, err := extractPayload(payload, filesDir, m.opts.MaxBackupBytes)
	if err == nil {
		// Drain the rest so a truncated or modified tail fails authentication.
		if _, cerr := io.Copy(io.Discard, payload); cerr != nil {
			err = ErrCorrupt
		}
	}
	if err != nil {
		return fail(CheckPayload, err)
	}
	if !ex.Manifest.matchesHeader(header) {
		return fail(CheckPayload, fmt.Errorf("%w: manifest does not match the header", ErrCorrupt))
	}
	res.Checks = append(res.Checks, Check{Code: CheckPayload, OK: true})
	_, hasConfig := ex.Paths[FileConfig]
	res.Manifest = &ManifestView{
		Name: ex.Manifest.Name, CreatedAt: ex.Manifest.CreatedAt, Trigger: ex.Manifest.Trigger,
		NezhaVersion: ex.Manifest.NezhaVersion, KeyID: ex.Manifest.KeyID, Tables: ex.Manifest.Tables, HasConfig: hasConfig,
	}

	// Origin. A backup that claims to come from this dashboard (its MAC key
	// id is ours, or it is encrypted to a key pair this dashboard created)
	// must carry a valid MAC; anything else can only be accepted after the
	// administrator explicitly acknowledges that its origin is unverifiable.
	authKey := s.authKey()
	macOK := len(authKey) > 0 && ex.MAC != "" && hmac.Equal([]byte(manifestMAC(authKey, ex.RawManifest)), []byte(ex.MAC))
	ours := len(authKey) > 0 && (ex.Manifest.AuthKeyID == authKeyID(authKey) || slices.Contains(s.ownKeyIDs(), header.KeyID))
	originVerified := macOK
	if ours && !macOK {
		return fail(CheckOrigin, errors.New("the backup claims to come from this dashboard but its authentication code does not match"))
	}
	if originVerified {
		res.Checks = append(res.Checks, Check{Code: CheckOrigin, OK: true})
	}

	if c, w := m.versionCheck(ex.Manifest.NezhaVersion); !c.OK {
		return fail(CheckVersion, errors.New(c.Message))
	} else {
		res.Checks = append(res.Checks, c)
		if w != nil {
			res.Warnings = append(res.Warnings, *w)
		}
	}
	insp, err := inspectDatabase(ex.Paths[FileDatabase])
	if err != nil || !insp.integrityOK {
		if err == nil {
			err = errors.New("the database failed the integrity check")
		}
		return fail(CheckDatabase, err)
	}
	res.Checks = append(res.Checks, Check{Code: CheckDatabase, OK: true})
	if !insp.hasAdmin {
		return fail(CheckAdmin, errors.New("the backup has no administrator account"))
	}
	res.Checks = append(res.Checks, Check{Code: CheckAdmin, OK: true})

	// Everything checked: stage it.
	if err := clearSessions(ex.Paths[FileDatabase]); err != nil {
		return nil, nil, err
	}
	files := map[string]string{}
	for name, path := range ex.Paths {
		sum, _, err := fileSHA256(path)
		if err != nil {
			return nil, nil, err
		}
		files[name] = sum
	}
	_ = os.Remove(backupPath)

	if !originVerified {
		res.Warnings = append(res.Warnings, Warning{Code: WarnOriginUnverified, RequireAck: true,
			Message: "This backup was not made by this dashboard (or its key changed); its origin cannot be verified."})
	}
	if !s.S3.Complete() || s.Encryption == nil {
		res.Warnings = append(res.Warnings, Warning{Code: WarnPreRestoreSkipped, RequireAck: true,
			Message: "Backups are not configured, so the current data cannot be backed up before it is replaced."})
	}
	if !hasConfig {
		res.Warnings = append(res.Warnings, Warning{Code: WarnConfigNotInBackup, Message: "The backup has no config.yaml; the current configuration is kept."})
	}
	res.Warnings = append(res.Warnings,
		Warning{Code: WarnTSDBNotRestored, Message: "Time series (TSDB) data is not part of backups and is left unchanged."},
		Warning{Code: WarnAgentsAfterBackup, Message: "Agents added after the backup was taken will be offline until added again."},
		Warning{Code: WarnRevokedCredentials, Message: "API tokens and OAuth grants revoked after the backup was taken become valid again."},
		Warning{Code: WarnSessionsCleared, Message: "All login sessions end; everyone must sign in again."},
	)

	token, err := randomHex(tokenBytes)
	if err != nil {
		return nil, nil, err
	}
	expires := m.opts.Now().Add(RestoreTokenTTL)
	stage := &restoreStage{
		token: token, userID: req.Actor.UserID, sessionID: req.Actor.SessionID,
		name: res.Name, sha256: res.SHA256, keyID: header.KeyID, header: header,
		dir: dir, files: files, expires: expires,
	}
	for _, w := range res.Warnings {
		if w.RequireAck {
			stage.requireAck = append(stage.requireAck, w.Code)
		}
	}
	res.RestoreToken = token
	res.ExpiresAt = &expires
	res.CanApply = true
	keep = true
	return res, stage, nil
}

func (m *Manager) download(ctx context.Context, s Settings, name, dst string) (string, error) {
	st, err := m.storage(s)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, backupTimeout)
	defer cancel()
	rc, info, err := st.Get(ctx, s.S3.Prefix+name+FileExt)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	if info.Size > m.opts.MaxBackupBytes {
		return "", ErrTooLarge
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) // #nosec G304 -- staging dir
	if err != nil {
		return "", err
	}
	hw := newHashingWriter(f)
	_, err = io.Copy(hw, io.LimitReader(rc, m.opts.MaxBackupBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if hw.n > m.opts.MaxBackupBytes {
		return "", ErrTooLarge
	}
	return hw.sum(), nil
}

// unlock returns the private key for header from the recovery key or the
// password. The password is tried on the wrapped key stored in the backup
// and, when the backup uses the current key pair, on the current wrapping
// (so a changed password also opens older backups on this dashboard).
func (m *Manager) unlock(h Header, password, recovery string, s Settings) (*age.X25519Identity, error) {
	if recovery != "" {
		id, err := ParseRecoveryKey(recovery)
		if err != nil {
			return nil, err
		}
		if identityKeyID(id) != h.KeyID {
			return nil, ErrKeyMismatch
		}
		return id, nil
	}
	if password == "" {
		return nil, errNoCredential
	}
	if id, err := UnwrapIdentity(h.WrappedIdentity, password); err == nil && identityKeyID(id) == h.KeyID {
		return id, nil
	}
	if s.Encryption != nil && s.Encryption.KeyID == h.KeyID && s.Encryption.WrappedIdentity != h.WrappedIdentity {
		if id, err := UnwrapIdentity(s.Encryption.WrappedIdentity, password); err == nil && identityKeyID(id) == h.KeyID {
			return id, nil
		}
	}
	return nil, ErrWrongPassword
}

func canonicalVersion(v string) string {
	v = strings.TrimSpace(v)
	if v != "" && !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// coreVersion returns the canonical vX.Y.Z of v without any pre-release or
// build suffix (-selfhost.N, -int.<sha>, ...), or "" if v is not a version.
func coreVersion(v string) string {
	c := semver.Canonical(canonicalVersion(v))
	if c == "" {
		return ""
	}
	if i := strings.IndexAny(c, "-+"); i >= 0 {
		c = c[:i]
	}
	return c
}

// versionCheck compares the version that made a backup with the running one
// by their core X.Y.Z only: suffixes (-selfhost.N, -int.<sha>) do not order
// reliably, e.g. commit hashes would be compared as text.
//   - newer core: refused, the database may use a structure this version
//     does not know;
//   - older core: allowed, the startup upgrades the database (informational);
//   - same core, different suffix: allowed after acknowledgement, a fork
//     patch release may carry small structure changes;
//   - not comparable (development builds): allowed after acknowledgement.
func (m *Manager) versionCheck(backupVersion string) (Check, *Warning) {
	bc, cc := coreVersion(backupVersion), coreVersion(m.opts.Version)
	if bc == "" || cc == "" {
		return Check{Code: CheckVersion, OK: true}, &Warning{Code: WarnVersionUnknown, RequireAck: true,
			Message: fmt.Sprintf("Cannot compare the backup version %q with the dashboard version %q.", backupVersion, m.opts.Version)}
	}
	switch semver.Compare(bc, cc) {
	case 1:
		return Check{Code: CheckVersion, OK: false, Message: fmt.Sprintf("the backup was made by a newer dashboard (%s); upgrade to at least that version first", backupVersion)}, nil
	case -1:
		return Check{Code: CheckVersion, OK: true}, &Warning{Code: WarnOlderVersion,
			Message: fmt.Sprintf("The backup was made by %s; its database is upgraded when the dashboard starts.", backupVersion)}
	}
	// Build identity: the whole version, including +build metadata, which
	// semver.Canonical would drop.
	if canonicalVersion(backupVersion) != canonicalVersion(m.opts.Version) {
		return Check{Code: CheckVersion, OK: true}, &Warning{Code: WarnVersionDiffers, RequireAck: true,
			Message: fmt.Sprintf("The backup was made by %s and this dashboard is %s: same release line, different build.", backupVersion, m.opts.Version)}
	}
	return Check{Code: CheckVersion, OK: true}, nil
}

// ApplyRequest applies a staged restore.
type ApplyRequest struct {
	Token        string
	Password     string
	RecoveryKey  string
	Confirm      string
	Acknowledged []string
	// Reauth verifies the administrator again (login password, or a recent
	// login for accounts without a password). It is called after the token
	// was validated.
	Reauth func() error
	Actor  Actor
}

// Apply re-verifies the administrator and the staged files, takes a
// pre-restore backup, records the restore for the next start and asks the
// process to restart. The swap itself happens in BootRestore before the
// database is opened.
func (m *Manager) Apply(ctx context.Context, req ApplyRequest) error {
	if err := m.checkRestarting(); err != nil {
		return err
	}
	if err := m.limiter.allow(req.Actor.UserID, "restore"); err != nil {
		return err
	}
	if !m.lockOp(ctx) {
		return ErrBusy
	}
	defer m.op.Unlock()
	m.restoreMu.Lock()
	defer m.restoreMu.Unlock()

	stage := m.stage
	if stage == nil || subtle.ConstantTimeCompare([]byte(stage.token), []byte(req.Token)) != 1 ||
		!m.opts.Now().Before(stage.expires) || stage.userID != req.Actor.UserID || stage.sessionID != req.Actor.SessionID {
		m.audit.record(auditEntry{Action: "restore_apply", Actor: req.Actor, Result: "failed", Error: ErrRestoreToken.Error()})
		return ErrRestoreToken
	}
	fail := func(err error) error {
		stage.failures++
		if stage.failures >= maxApplyFailures {
			m.dropStageLocked()
		}
		m.audit.record(auditEntry{Action: "restore_apply", Actor: req.Actor, Name: stage.name, SHA256: stage.sha256, Result: "failed", Error: err.Error()})
		return err
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Confirm)), []byte(ConfirmWord(stage.name))) != 1 {
		return fail(ErrConfirmWord)
	}
	for _, code := range stage.requireAck {
		if !slices.Contains(req.Acknowledged, code) {
			return fail(ErrAckRequired)
		}
	}
	if req.Reauth == nil {
		return fail(errors.New("re-authentication is required"))
	}
	if err := req.Reauth(); err != nil {
		return fail(err)
	}
	s, _ := m.effective()
	if _, err := m.unlock(stage.header, req.Password, req.RecoveryKey, s); err != nil {
		return fail(err)
	}
	for name, want := range stage.files {
		got, _, err := fileSHA256(filepath.Join(stage.dir, stageFilesDir, name))
		if err != nil || got != want {
			m.dropStageLocked()
			m.audit.record(auditEntry{Action: "restore_apply", Actor: req.Actor, Name: stage.name, SHA256: stage.sha256, Result: "failed", Error: "staged files changed"})
			return ErrCorrupt
		}
	}

	if s.S3.Complete() && s.Encryption != nil {
		name, err := NewName(m.opts.Now(), TriggerPreRestore)
		if err != nil {
			return err
		}
		info, err := m.runBackup(ctx, name, s)
		m.audit.record(auditEntry{Action: "backup", Actor: req.Actor, Name: name, SHA256: info.SHA256, Result: resultOf(err), Error: errString(err)})
		if err != nil {
			return fmt.Errorf("pre-restore backup failed, nothing was changed: %w", err)
		}
		m.applyRetention(ctx, s, TriggerPreRestore, PreRestoreKeep)
	}

	marker := pendingRestore{
		Dir: filepath.Base(stage.dir), Files: stage.files, Name: stage.name, SHA256: stage.sha256,
		UserID: req.Actor.UserID, PreparedAt: m.opts.Now().UTC(),
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	if err := writeFileDurable(filepath.Join(m.opts.DataDir, pendingDirName, pendingFileName), data); err != nil {
		return err
	}
	m.stage = nil
	m.restarting = true
	m.audit.record(auditEntry{Action: "restore_apply", Actor: req.Actor, Name: stage.name, SHA256: stage.sha256, Result: "ok"})
	if m.opts.Restart != nil {
		time.AfterFunc(defaultApplyRestartWait, m.opts.Restart)
	}
	return nil
}

// Cancel discards the staged restore identified by token.
func (m *Manager) Cancel(token string, actor Actor) {
	m.restoreMu.Lock()
	defer m.restoreMu.Unlock()
	if m.stage != nil && subtle.ConstantTimeCompare([]byte(m.stage.token), []byte(token)) == 1 && m.stage.userID == actor.UserID {
		m.audit.record(auditEntry{Action: "restore_cancel", Actor: actor, Name: m.stage.name, SHA256: m.stage.sha256, Result: "ok"})
		m.dropStageLocked()
	}
}

func (m *Manager) expireStage(stage *restoreStage) {
	m.restoreMu.Lock()
	defer m.restoreMu.Unlock()
	if m.stage == stage && !m.restarting {
		m.dropStageLocked()
	}
}

func (m *Manager) dropStageLocked() {
	if m.stage != nil {
		_ = os.RemoveAll(m.stage.dir)
		m.stage = nil
	}
}

// RestoreStatus returns the result of the last restore, if any.
func (m *Manager) RestoreStatus() (*RestoreResult, error) {
	return readRestoreResult(m.opts.DataDir)
}
