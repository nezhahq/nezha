package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/nezhahq/nezha/pkg/backup/s3store"
)

// Directory and file names below Options.DataDir.
const (
	tmpDirName      = ".backup-tmp"
	pendingDirName  = "restore-pending"
	rollbackDirName = "restore-rollback"
	pendingFileName = "pending.json"
	stateFileName   = "restore-state.json"
	commitFileName  = "restore-committed.json"
	resultFileName  = "restore-result.json"
	auditFileName   = "backup-audit.log"
)

const (
	// DefaultMaxBackupBytes caps a downloaded or uploaded backup file and the
	// size of its decrypted payload.
	DefaultMaxBackupBytes int64 = 256 << 20
	// PreRestoreKeep is how many pre-restore backups retention keeps.
	PreRestoreKeep = 3
	// ResetConfirmWord must be sent to reset the backup password.
	ResetConfirmWord = "RESET"

	maxFailedJobs = 10
	listCacheTTL  = 30 * time.Second
	backupTimeout = 30 * time.Minute
	uploadRetries = 3
)

var (
	ErrNotConfigured     = errors.New("backup storage and password must be configured first")
	ErrStorageMissing    = errors.New("backup storage is not configured")
	ErrBusy              = errors.New("another backup or restore operation is in progress")
	ErrRestarting        = errors.New("the dashboard is restarting to apply a restore")
	ErrInvalidName       = errors.New("invalid backup name")
	ErrPasswordSet       = errors.New("backup password is already set; change or reset it instead")
	ErrPasswordNotSet    = errors.New("backup password is not set")
	ErrResetConfirm      = errors.New("type RESET to confirm resetting the backup password")
	ErrSecretRequired    = errors.New("re-enter the secret access key when changing the endpoint or bucket")
	ErrDisabled          = errors.New("backups are unavailable: the database is not a file")
	ErrInvalidCron       = errors.New("invalid backup schedule")
	ErrInvalidRetention  = fmt.Errorf("retention must be between 1 and %d", MaxRetention)
	ErrLockedByEnv       = errors.New("this setting is managed by an environment variable")
	ErrInvalidBucketName = errors.New("invalid bucket name")
	ErrRegionTooLong     = errors.New("region is too long")
)

// LockedError reports a setting that is managed by an environment variable.
type LockedError struct{ Field string }

func (e *LockedError) Error() string { return e.Field + ": " + ErrLockedByEnv.Error() }

// Is makes errors.Is(err, ErrLockedByEnv) hold.
func (e *LockedError) Is(target error) bool { return target == ErrLockedByEnv }

// Options configures a Manager.
type Options struct {
	// DataDir is the directory of the database file. Temporary files,
	// staged restores, restore results and the audit log live here, so a
	// Docker deployment that only mounts the data directory needs nothing new.
	DataDir      string
	ConfigPath   string
	SettingsPath string
	Version      string
	// Snapshot writes a consistent copy of the live database to dst.
	Snapshot   func(ctx context.Context, dst string) error
	CronParser cron.ScheduleParser
	Location   *time.Location
	// NewStorage builds the storage for the given settings (tests replace it).
	NewStorage func(S3Settings) (Storage, error)
	// Restart asks the process to shut down gracefully so that its
	// supervisor starts it again and the staged restore is applied.
	Restart        func()
	MaxBackupBytes int64
	Now            func() time.Time
	// AllowLoopbackHTTP accepts http:// endpoints on loopback addresses.
	// Integration tests against a local MinIO only; the dashboard never sets it.
	AllowLoopbackHTTP bool
	// RetryDelay is the base delay between upload attempts.
	RetryDelay time.Duration
}

// Manager runs backups and restores.
type Manager struct {
	opts Options

	mu       sync.Mutex
	settings Settings // file values; env overrides are applied on read
	cron     *cron.Cron
	entry    cron.EntryID

	op         sync.Mutex // one backup / prepare / apply at a time
	restarting bool

	jobsMu sync.Mutex
	jobs   []BackupInfo // running and recently failed backups

	cacheMu   sync.Mutex
	listCache []BackupInfo
	listAt    time.Time
	statCache map[string]ObjectInfo

	restoreMu sync.Mutex
	stage     *restoreStage

	limiter *rateLimiter
	audit   *auditLog
}

// NewManager loads the settings and starts the schedule.
func NewManager(opts Options) (*Manager, error) {
	if opts.DataDir == "" {
		return nil, ErrDisabled
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.CronParser == nil {
		opts.CronParser = cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	}
	if opts.NewStorage == nil {
		allow := opts.AllowLoopbackHTTP
		opts.NewStorage = func(s S3Settings) (Storage, error) { return newS3Storage(s, allow) }
	}
	if opts.MaxBackupBytes <= 0 {
		opts.MaxBackupBytes = DefaultMaxBackupBytes
	}
	if opts.RetryDelay <= 0 {
		opts.RetryDelay = 2 * time.Second
	}
	if opts.SettingsPath == "" {
		opts.SettingsPath = filepath.Join(filepath.Dir(opts.ConfigPath), SettingsFileName)
	}
	s, err := loadSettings(opts.SettingsPath)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", opts.SettingsPath, err)
	}
	s.applyDefaults()
	m := &Manager{
		opts:      opts,
		settings:  s,
		statCache: map[string]ObjectInfo{},
		limiter:   newRateLimiter(opts.Now),
		audit:     newAuditLog(filepath.Join(opts.DataDir, auditFileName), opts.Now),
	}
	// Leftovers of a backup interrupted by a crash or restart.
	_ = os.RemoveAll(filepath.Join(opts.DataDir, tmpDirName))
	m.cron = cron.New(cron.WithParser(opts.CronParser), cron.WithLocation(opts.Location))
	if err := m.reschedule(s.Schedule); err != nil {
		log.Printf("NEZHA>> backup: schedule %q disabled: %v", s.Schedule.Cron, err)
	}
	m.cron.Start()
	return m, nil
}

// Close stops the schedule.
func (m *Manager) Close() {
	if m.cron != nil {
		<-m.cron.Stop().Done()
	}
}

func (m *Manager) effective() (Settings, map[string]bool) {
	m.mu.Lock()
	s := m.settings.clone()
	m.mu.Unlock()
	locked := envOverrides(&s.S3)
	if p, err := NormalizePrefix(s.S3.Prefix); err == nil {
		s.S3.Prefix = p
	}
	return s, locked
}

func (m *Manager) storage(s Settings) (Storage, error) {
	if !s.S3.Complete() {
		return nil, ErrStorageMissing
	}
	return m.opts.NewStorage(s.S3)
}

// ---- configuration ----

// ScheduleView is the schedule part of ConfigView.
type ScheduleView struct {
	Enabled   bool       `json:"enabled"`
	Cron      string     `json:"cron"`
	Retention int        `json:"retention"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
}

// S3View is the S3 part of ConfigView. It never carries the secret key, and
// only the last four characters of the access key id.
type S3View struct {
	Endpoint           string   `json:"endpoint"`
	Bucket             string   `json:"bucket"`
	Region             string   `json:"region"`
	Prefix             string   `json:"prefix"`
	UsePathStyle       bool     `json:"use_path_style"`
	AccessKeyIDMasked  string   `json:"access_key_id_masked"`
	SecretAccessKeySet bool     `json:"secret_access_key_set"`
	EnvLocked          []string `json:"env_locked"`
}

// EncryptionView describes the current key pair.
type EncryptionView struct {
	KeyID     string    `json:"key_id"`
	CreatedAt time.Time `json:"created_at"`
}

// ConfigView is what the API returns for the backup configuration.
type ConfigView struct {
	Configured        bool            `json:"configured"`
	StorageConfigured bool            `json:"storage_configured"`
	Schedule          ScheduleView    `json:"schedule"`
	S3                S3View          `json:"s3"`
	Encryption        *EncryptionView `json:"encryption"`
	UploadMaxBytes    int64           `json:"upload_max_bytes"`
	PasswordMinLength int             `json:"password_min_length"`
	ConfirmWordLength int             `json:"confirm_word_length"`
	ResetConfirmWord  string          `json:"reset_confirm_word"`
}

func maskKeyID(id string) string {
	if id == "" {
		return ""
	}
	if len(id) <= 4 {
		return "****"
	}
	return "****" + id[len(id)-4:]
}

// View returns the configuration without secrets.
func (m *Manager) View() ConfigView {
	s, locked := m.effective()
	v := ConfigView{
		Configured:        s.S3.Complete() && s.Encryption != nil,
		StorageConfigured: s.S3.Complete(),
		Schedule:          ScheduleView{Enabled: s.Schedule.Enabled, Cron: s.Schedule.Cron, Retention: s.Schedule.Retention},
		S3: S3View{
			Endpoint:           s.S3.Endpoint,
			Bucket:             s.S3.Bucket,
			Region:             s.S3.Region,
			Prefix:             s.S3.Prefix,
			UsePathStyle:       s.S3.PathStyle(),
			AccessKeyIDMasked:  maskKeyID(s.S3.AccessKeyID),
			SecretAccessKeySet: s.S3.SecretAccessKey != "",
			EnvLocked:          []string{},
		},
		UploadMaxBytes:    m.opts.MaxBackupBytes,
		PasswordMinLength: MinPasswordLength,
		ConfirmWordLength: ConfirmWordLength,
		ResetConfirmWord:  ResetConfirmWord,
	}
	for k := range locked {
		v.S3.EnvLocked = append(v.S3.EnvLocked, k)
	}
	slices.Sort(v.S3.EnvLocked)
	if s.Encryption != nil {
		v.Encryption = &EncryptionView{KeyID: s.Encryption.KeyID, CreatedAt: s.Encryption.CreatedAt}
	}
	m.mu.Lock()
	if m.entry != 0 {
		if next := m.cron.Entry(m.entry).Next; !next.IsZero() {
			v.Schedule.NextRunAt = &next
		}
	}
	m.mu.Unlock()
	return v
}

// ScheduleUpdate changes the schedule; nil fields are left unchanged.
type ScheduleUpdate struct {
	Enabled   *bool   `json:"enabled,omitempty"`
	Cron      *string `json:"cron,omitempty"`
	Retention *int    `json:"retention,omitempty"`
}

// S3Update changes the storage target; nil fields are left unchanged. An
// empty secret_access_key or access_key_id keeps the saved value.
type S3Update struct {
	Endpoint        *string `json:"endpoint,omitempty"`
	Bucket          *string `json:"bucket,omitempty"`
	Region          *string `json:"region,omitempty"`
	Prefix          *string `json:"prefix,omitempty"`
	UsePathStyle    *bool   `json:"use_path_style,omitempty"`
	AccessKeyID     *string `json:"access_key_id,omitempty"`
	SecretAccessKey *string `json:"secret_access_key,omitempty"`
}

// ConfigUpdate is the body of a configuration change.
type ConfigUpdate struct {
	Schedule *ScheduleUpdate `json:"schedule,omitempty"`
	S3       *S3Update       `json:"s3,omitempty"`
}

func validBucket(b string) bool {
	if len(b) < 3 || len(b) > 63 {
		return false
	}
	for _, r := range b {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_') {
			return false
		}
	}
	return true
}

// mergeS3 applies u to cur (the file values) and validates the result.
// effective is cur with environment overrides applied, used for the
// "changing endpoint or bucket needs the secret again" rule.
func mergeS3(cur S3Settings, effective S3Settings, locked map[string]bool, u *S3Update, allowLoopbackHTTP bool) (S3Settings, error) {
	if u == nil {
		return cur, nil
	}
	str := func(field string, in *string, dst *string, effectiveVal string) error {
		if in == nil {
			return nil
		}
		v := strings.TrimSpace(*in)
		if locked[field] {
			if v != "" && v != effectiveVal {
				return &LockedError{Field: field}
			}
			return nil
		}
		*dst = v
		return nil
	}
	next := cur
	if err := str("endpoint", u.Endpoint, &next.Endpoint, effective.Endpoint); err != nil {
		return cur, err
	}
	if err := str("bucket", u.Bucket, &next.Bucket, effective.Bucket); err != nil {
		return cur, err
	}
	if err := str("region", u.Region, &next.Region, effective.Region); err != nil {
		return cur, err
	}
	if u.Prefix != nil && !locked["prefix"] {
		p, err := NormalizePrefix(*u.Prefix)
		if err != nil {
			return cur, err
		}
		next.Prefix = p
	} else if u.Prefix != nil {
		if p, _ := NormalizePrefix(*u.Prefix); p != effective.Prefix {
			return cur, &LockedError{Field: "prefix"}
		}
	}
	if u.UsePathStyle != nil {
		v := *u.UsePathStyle
		next.UsePathStyle = &v
	}
	if u.AccessKeyID != nil && strings.TrimSpace(*u.AccessKeyID) != "" {
		if err := str("access_key_id", u.AccessKeyID, &next.AccessKeyID, effective.AccessKeyID); err != nil {
			return cur, err
		}
	}
	secretGiven := u.SecretAccessKey != nil && *u.SecretAccessKey != ""
	if secretGiven {
		if locked["secret_access_key"] {
			return cur, &LockedError{Field: "secret_access_key"}
		}
		next.SecretAccessKey = *u.SecretAccessKey
	}
	// A stolen session must not be able to send the saved credentials to
	// another server: moving the target requires the secret again.
	moved := (effective.Endpoint != "" && next.Endpoint != effective.Endpoint && !locked["endpoint"]) ||
		(effective.Bucket != "" && next.Bucket != effective.Bucket && !locked["bucket"])
	if moved && !secretGiven {
		return cur, ErrSecretRequired
	}
	if next.Endpoint != "" {
		if _, _, err := s3store.ParseEndpoint(next.Endpoint, allowLoopbackHTTP); err != nil {
			return cur, err
		}
	}
	if next.Bucket != "" && !validBucket(next.Bucket) {
		return cur, ErrInvalidBucketName
	}
	if len(next.Region) > 64 {
		return cur, ErrRegionTooLong
	}
	if next.Region == "" {
		next.Region = DefaultRegion
	}
	return next, nil
}

// UpdateConfig applies a configuration change and persists it.
func (m *Manager) UpdateConfig(u ConfigUpdate, actor Actor) (ConfigView, error) {
	if err := m.checkRestarting(); err != nil {
		return ConfigView{}, err
	}
	eff, locked := m.effective()
	m.mu.Lock()
	next := m.settings.clone()
	s3, err := mergeS3(next.S3, eff.S3, locked, u.S3, m.opts.AllowLoopbackHTTP)
	if err != nil {
		m.mu.Unlock()
		return ConfigView{}, err
	}
	next.S3 = s3
	if u.Schedule != nil {
		if u.Schedule.Enabled != nil {
			next.Schedule.Enabled = *u.Schedule.Enabled
		}
		if u.Schedule.Cron != nil {
			next.Schedule.Cron = strings.TrimSpace(*u.Schedule.Cron)
		}
		if u.Schedule.Retention != nil {
			next.Schedule.Retention = *u.Schedule.Retention
		}
		if next.Schedule.Retention < 1 || next.Schedule.Retention > MaxRetention {
			m.mu.Unlock()
			return ConfigView{}, ErrInvalidRetention
		}
		if _, err := m.opts.CronParser.Parse(next.Schedule.Cron); err != nil {
			m.mu.Unlock()
			return ConfigView{}, fmt.Errorf("%w: %v", ErrInvalidCron, err)
		}
	}
	if err := saveSettings(m.opts.SettingsPath, next); err != nil {
		m.mu.Unlock()
		return ConfigView{}, err
	}
	m.settings = next
	err = m.rescheduleLocked(next.Schedule)
	m.mu.Unlock()
	m.invalidateList()
	m.audit.record(auditEntry{Action: "config_update", Actor: actor, Result: resultOf(err)})
	if err != nil {
		return ConfigView{}, err
	}
	return m.View(), nil
}

// TestConnection checks that the (possibly unsaved) storage settings can
// write, read and delete an object. The same secret rule as UpdateConfig
// applies, so the test cannot send saved credentials to another endpoint.
func (m *Manager) TestConnection(ctx context.Context, u *S3Update, actor Actor) error {
	eff, locked := m.effective()
	m.mu.Lock()
	cur := m.settings.S3
	m.mu.Unlock()
	candidate, err := mergeS3(cur, eff.S3, locked, u, m.opts.AllowLoopbackHTTP)
	if err != nil {
		return err
	}
	// Re-apply environment overrides on top of the candidate.
	envOverrides(&candidate)
	if p, err := NormalizePrefix(candidate.Prefix); err == nil {
		candidate.Prefix = p
	}
	if !candidate.Complete() {
		return ErrStorageMissing
	}
	st, err := m.opts.NewStorage(candidate)
	if err == nil {
		err = probeStorage(ctx, st, candidate.Prefix)
	}
	m.audit.record(auditEntry{Action: "storage_test", Actor: actor, Result: resultOf(err)})
	return err
}

func probeStorage(ctx context.Context, st Storage, prefix string) error {
	ctx, cancel := context.WithTimeout(ctx, s3store.DefaultTimeout)
	defer cancel()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	key := prefix + ".nezha-probe-" + hex.EncodeToString(b[:])
	body := []byte("nezha backup connectivity probe")
	if _, err := st.Put(ctx, key, strings.NewReader(string(body)), int64(len(body)), nil); err != nil {
		return err
	}
	defer func() { _ = st.Delete(context.WithoutCancel(ctx), key) }()
	if info, err := st.Stat(ctx, key); err != nil {
		return err
	} else if info.Size != int64(len(body)) {
		return errors.New("storage returned a different object size")
	}
	rc, _, err := st.Get(ctx, key)
	if err != nil {
		return err
	}
	got, err := io.ReadAll(io.LimitReader(rc, 1024))
	_ = rc.Close()
	if err != nil {
		return err
	}
	if string(got) != string(body) {
		return errors.New("storage returned different content")
	}
	return st.Delete(ctx, key)
}

func (m *Manager) reschedule(s ScheduleSettings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rescheduleLocked(s)
}

func (m *Manager) rescheduleLocked(s ScheduleSettings) error {
	if m.entry != 0 {
		m.cron.Remove(m.entry)
		m.entry = 0
	}
	if !s.Enabled {
		return nil
	}
	id, err := m.cron.AddFunc(s.Cron, m.scheduledBackup)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCron, err)
	}
	m.entry = id
	return nil
}

func (m *Manager) scheduledBackup() {
	if _, err := m.StartBackup(TriggerSchedule, Actor{System: true}); err != nil {
		log.Printf("NEZHA>> backup: scheduled backup skipped: %v", err)
	}
}

// ---- password ----

// ErrReauthRequired is returned when a password operation is called without
// a re-authentication check.
var ErrReauthRequired = errors.New("re-authentication is required")

// verifyPasswordActor re-authenticates the administrator before the key pair
// is created or replaced. Whoever sets the password can decrypt every later
// backup (database and config.yaml with their secrets), so a stolen session
// alone must not be enough, as for a restore. Attempts are rate limited.
func (m *Manager) verifyPasswordActor(reauth func() error, actor Actor) error {
	if err := m.limiter.allow(actor.UserID, "password"); err != nil {
		return err
	}
	if reauth == nil {
		return ErrReauthRequired
	}
	if err := reauth(); err != nil {
		m.audit.record(auditEntry{Action: "password_reauth", Actor: actor, Result: "failed", Error: err.Error()})
		return err
	}
	return nil
}

// SetPassword sets the backup password for the first time and returns the
// recovery key, which is never stored. reauth verifies the administrator
// again (see verifyPasswordActor).
func (m *Manager) SetPassword(password string, reauth func() error, actor Actor) (string, EncryptionView, error) {
	if err := m.checkRestarting(); err != nil {
		return "", EncryptionView{}, err
	}
	if err := ValidatePassword(password); err != nil {
		return "", EncryptionView{}, err
	}
	if err := m.verifyPasswordActor(reauth, actor); err != nil {
		return "", EncryptionView{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settings.Encryption != nil {
		return "", EncryptionView{}, ErrPasswordSet
	}
	return m.newKeyLocked(password, "password_set", actor)
}

// ResetPassword replaces the key pair. Earlier backups then need the old
// password or the old recovery key. reauth verifies the administrator again
// (see verifyPasswordActor).
func (m *Manager) ResetPassword(password, confirm string, reauth func() error, actor Actor) (string, EncryptionView, error) {
	if err := m.checkRestarting(); err != nil {
		return "", EncryptionView{}, err
	}
	if confirm != ResetConfirmWord {
		return "", EncryptionView{}, ErrResetConfirm
	}
	if err := ValidatePassword(password); err != nil {
		return "", EncryptionView{}, err
	}
	if err := m.verifyPasswordActor(reauth, actor); err != nil {
		return "", EncryptionView{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settings.Encryption == nil {
		return "", EncryptionView{}, ErrPasswordNotSet
	}
	return m.newKeyLocked(password, "password_reset", actor)
}

func (m *Manager) newKeyLocked(password, action string, actor Actor) (string, EncryptionView, error) {
	km, recovery, err := NewKeyMaterial(password, m.opts.Now())
	if err != nil {
		return "", EncryptionView{}, err
	}
	next := m.settings.clone()
	next.Encryption = &km
	if !slices.Contains(next.KnownKeyIDs, km.KeyID) {
		next.KnownKeyIDs = append(next.KnownKeyIDs, km.KeyID)
	}
	if err := next.ensureAuthKey(); err != nil {
		return "", EncryptionView{}, err
	}
	if err := saveSettings(m.opts.SettingsPath, next); err != nil {
		return "", EncryptionView{}, err
	}
	m.settings = next
	m.audit.record(auditEntry{Action: action, Actor: actor, KeyID: km.KeyID, Result: "ok"})
	m.invalidateList()
	return recovery, EncryptionView{KeyID: km.KeyID, CreatedAt: km.CreatedAt}, nil
}

// ChangePassword re-wraps the private key with a new password.
func (m *Manager) ChangePassword(oldPassword, newPassword string, actor Actor) (EncryptionView, error) {
	if err := m.checkRestarting(); err != nil {
		return EncryptionView{}, err
	}
	if err := m.limiter.allow(actor.UserID, "password"); err != nil {
		return EncryptionView{}, err
	}
	if err := ValidatePassword(newPassword); err != nil {
		return EncryptionView{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settings.Encryption == nil {
		return EncryptionView{}, ErrPasswordNotSet
	}
	km, err := m.settings.Encryption.Rewrap(oldPassword, newPassword)
	if err != nil {
		m.audit.record(auditEntry{Action: "password_change", Actor: actor, Result: "failed", Error: err.Error()})
		return EncryptionView{}, err
	}
	next := m.settings.clone()
	next.Encryption = &km
	if err := saveSettings(m.opts.SettingsPath, next); err != nil {
		return EncryptionView{}, err
	}
	m.settings = next
	m.audit.record(auditEntry{Action: "password_change", Actor: actor, KeyID: km.KeyID, Result: "ok"})
	return EncryptionView{KeyID: km.KeyID, CreatedAt: km.CreatedAt}, nil
}

// ---- backups ----

// BackupInfo is one entry of the backup list.
type BackupInfo struct {
	Name              string    `json:"name"`
	Size              int64     `json:"size"`
	SHA256            string    `json:"sha256"`
	Trigger           Trigger   `json:"trigger"`
	Status            string    `json:"status"`
	Error             string    `json:"error,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	NezhaVersion      string    `json:"nezha_version,omitempty"`
	KeyID             string    `json:"key_id,omitempty"`
	KeyMatchesCurrent bool      `json:"key_matches_current"`
}

// Backup statuses.
const (
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusFailed  = "failed"
)

func (m *Manager) checkRestarting() error {
	m.restoreMu.Lock()
	defer m.restoreMu.Unlock()
	if m.restarting {
		return ErrRestarting
	}
	return nil
}

// StartBackup starts a backup in the background and returns its running entry.
func (m *Manager) StartBackup(trigger Trigger, actor Actor) (BackupInfo, error) {
	if err := m.checkRestarting(); err != nil {
		return BackupInfo{}, err
	}
	s, _ := m.effective()
	if !s.S3.Complete() || s.Encryption == nil {
		return BackupInfo{}, ErrNotConfigured
	}
	if !m.op.TryLock() {
		return BackupInfo{}, ErrBusy
	}
	name, err := NewName(m.opts.Now(), trigger)
	if err != nil {
		m.op.Unlock()
		return BackupInfo{}, err
	}
	created, _, _ := ParseName(name)
	job := BackupInfo{Name: name, Trigger: trigger, Status: StatusRunning, CreatedAt: created, NezhaVersion: m.opts.Version, KeyID: s.Encryption.KeyID, KeyMatchesCurrent: true}
	m.setJob(job)
	go func() {
		defer m.op.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), backupTimeout)
		defer cancel()
		info, err := m.runBackup(ctx, name, s)
		m.finishJob(job, info, err)
		m.audit.record(auditEntry{Action: "backup", Actor: actor, Name: name, SHA256: info.SHA256, Result: resultOf(err), Error: errString(err)})
		if err != nil {
			log.Printf("NEZHA>> backup %s failed: %v", name, err)
			return
		}
		log.Printf("NEZHA>> backup %s uploaded (%d bytes)", name, info.Size)
		if trigger == TriggerSchedule {
			m.applyRetention(ctx, s, TriggerSchedule, s.Schedule.Retention)
		}
	}()
	return job, nil
}

// runBackup takes the snapshot, encrypts it and uploads it. The caller holds m.op.
func (m *Manager) runBackup(ctx context.Context, name string, s Settings) (BackupInfo, error) {
	created, trigger, _ := ParseName(name)
	st, err := m.storage(s)
	if err != nil {
		return BackupInfo{}, err
	}
	if s.Encryption == nil {
		return BackupInfo{}, ErrPasswordNotSet
	}
	tmp := filepath.Join(m.opts.DataDir, tmpDirName, name)
	if err := os.MkdirAll(tmp, 0700); err != nil {
		return BackupInfo{}, err
	}
	defer os.RemoveAll(tmp)

	dbPath := filepath.Join(tmp, FileDatabase)
	if err := m.opts.Snapshot(ctx, dbPath); err != nil {
		return BackupInfo{}, fmt.Errorf("database snapshot: %w", err)
	}
	insp, err := inspectDatabase(dbPath)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("database snapshot: %w", err)
	}
	if !insp.integrityOK || !insp.hasAdmin {
		return BackupInfo{}, errors.New("database snapshot failed the integrity check")
	}
	sources := map[string]string{FileDatabase: dbPath}
	if m.opts.ConfigPath != "" {
		if _, err := os.Stat(m.opts.ConfigPath); err == nil {
			sources[FileConfig] = m.opts.ConfigPath
		}
	}
	// A restore refuses payloads whose unpacked size exceeds the limit, so a
	// backup that could not be restored must not be produced at all. Check
	// early (compression could hide it) and again exactly after packing.
	var unpacked int64
	for _, path := range sources {
		if st, err := os.Stat(path); err == nil {
			unpacked += st.Size()
		}
	}
	if unpacked > m.opts.MaxBackupBytes {
		return BackupInfo{}, m.tooLarge(unpacked)
	}

	header := Header{
		Format:          formatVersion,
		Name:            name,
		CreatedAt:       created,
		Trigger:         trigger,
		NezhaVersion:    m.opts.Version,
		KeyID:           s.Encryption.KeyID,
		WrappedIdentity: s.Encryption.WrappedIdentity,
	}
	manifest := &Manifest{
		Format: formatVersion, Name: name, CreatedAt: created, Trigger: trigger,
		NezhaVersion: m.opts.Version, KeyID: s.Encryption.KeyID, Tables: insp.tables,
		AuthKeyID: authKeyID(s.authKey()),
	}
	outPath := filepath.Join(tmp, name+FileExt)
	sum, size, unpacked, err := writeBackupFile(outPath, header, s.Encryption.Recipient, manifest, sources, s.authKey())
	if err != nil {
		return BackupInfo{}, err
	}
	if unpacked > m.opts.MaxBackupBytes {
		return BackupInfo{}, m.tooLarge(unpacked)
	}
	if size > m.opts.MaxBackupBytes {
		return BackupInfo{}, m.tooLarge(size)
	}
	meta := map[string]string{metaSHA256: sum, metaVersion: m.opts.Version, metaTrigger: string(trigger), metaKeyID: s.Encryption.KeyID}
	key := s.S3.Prefix + name + FileExt
	if err := m.upload(ctx, st, key, outPath, size, meta); err != nil {
		return BackupInfo{}, err
	}
	m.invalidateList()
	return BackupInfo{
		Name: name, Size: size, SHA256: sum, Trigger: trigger, Status: StatusSuccess, CreatedAt: created,
		NezhaVersion: m.opts.Version, KeyID: s.Encryption.KeyID, KeyMatchesCurrent: true,
	}, nil
}

// tooLarge is the error of a backup that would exceed the size limit shared
// by backup, download, upload and restore.
func (m *Manager) tooLarge(n int64) error {
	return fmt.Errorf("%w: %d bytes, the limit is %d bytes", ErrTooLarge, n, m.opts.MaxBackupBytes)
}

// writeBackupFile returns the checksum and size of the file and the
// unpacked size of its payload.
func writeBackupFile(path string, h Header, recipient string, manifest *Manifest, sources map[string]string, authKey []byte) (string, int64, int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) // #nosec G304 -- dashboard temp dir
	if err != nil {
		return "", 0, 0, err
	}
	var unpacked int64
	hw := newHashingWriter(f)
	w, err := encryptTo(hw, h, recipient)
	if err == nil {
		unpacked, err = writePayload(w, manifest, sources, authKey)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, 0, err
	}
	return hw.sum(), hw.n, unpacked, nil
}

func (m *Manager) upload(ctx context.Context, st Storage, key, path string, size int64, meta map[string]string) error {
	var err error
	for attempt := range uploadRetries {
		if attempt > 0 {
			select {
			case <-time.After(m.opts.RetryDelay << (attempt - 1)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err = m.uploadOnce(ctx, st, key, path, size, meta)
		if err == nil {
			return nil
		}
		log.Printf("NEZHA>> backup: upload attempt %d failed: %v", attempt+1, err)
	}
	return err
}

func (m *Manager) uploadOnce(ctx context.Context, st Storage, key, path string, size int64, meta map[string]string) error {
	f, err := os.Open(path) // #nosec G304 -- dashboard temp dir
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := st.Put(ctx, key, f, size, meta); err != nil {
		return err
	}
	info, err := st.Stat(ctx, key)
	if err != nil {
		return err
	}
	if info.Size != size {
		return fmt.Errorf("uploaded object has %d bytes, expected %d", info.Size, size)
	}
	return nil
}

func (m *Manager) setJob(job BackupInfo) {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	m.jobs = slices.DeleteFunc(m.jobs, func(j BackupInfo) bool { return j.Name == job.Name })
	m.jobs = append(m.jobs, job)
}

func (m *Manager) finishJob(job, info BackupInfo, err error) {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	m.jobs = slices.DeleteFunc(m.jobs, func(j BackupInfo) bool { return j.Name == job.Name })
	if err != nil {
		job.Status = StatusFailed
		job.Error = err.Error()
		m.jobs = append(m.jobs, job)
		failed := 0
		for i := len(m.jobs) - 1; i >= 0; i-- {
			if m.jobs[i].Status == StatusFailed {
				failed++
				if failed > maxFailedJobs {
					m.jobs = append(m.jobs[:i], m.jobs[i+1:]...)
				}
			}
		}
	}
	m.invalidateList()
}

func (m *Manager) invalidateList() {
	m.cacheMu.Lock()
	m.listCache = nil
	m.cacheMu.Unlock()
}

// List returns the stored backups merged with running and failed ones.
func (m *Manager) List(ctx context.Context) ([]BackupInfo, error) {
	s, _ := m.effective()
	var stored []BackupInfo
	if s.S3.Complete() {
		var err error
		stored, err = m.cachedList(ctx, s)
		if err != nil {
			return nil, err
		}
	}
	out := slices.Clone(stored)
	present := map[string]bool{}
	for _, b := range stored {
		present[b.Name] = true
	}
	m.jobsMu.Lock()
	for _, j := range m.jobs {
		if !present[j.Name] {
			out = append(out, j)
		}
	}
	m.jobsMu.Unlock()
	current := ""
	if s.Encryption != nil {
		current = s.Encryption.KeyID
	}
	for i := range out {
		out[i].KeyMatchesCurrent = out[i].KeyID != "" && out[i].KeyID == current
	}
	slices.SortFunc(out, func(a, b BackupInfo) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

func (m *Manager) cachedList(ctx context.Context, s Settings) ([]BackupInfo, error) {
	m.cacheMu.Lock()
	if m.listCache != nil && m.opts.Now().Sub(m.listAt) < listCacheTTL {
		out := m.listCache
		m.cacheMu.Unlock()
		return out, nil
	}
	m.cacheMu.Unlock()
	out, err := m.listStored(ctx, s)
	if err != nil {
		return nil, err
	}
	m.cacheMu.Lock()
	m.listCache = out
	m.listAt = m.opts.Now()
	m.cacheMu.Unlock()
	return out, nil
}

// listStored lists the backup objects below the prefix. Objects whose key is
// not a backup name are ignored.
func (m *Manager) listStored(ctx context.Context, s Settings) ([]BackupInfo, error) {
	st, err := m.storage(s)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, s3store.DefaultTimeout)
	defer cancel()
	objs, err := st.List(ctx, s.S3.Prefix)
	if err != nil {
		return nil, err
	}
	var out []BackupInfo
	listed := map[string]bool{}
	for _, o := range objs {
		name, ok := strings.CutPrefix(o.Key, s.S3.Prefix)
		if !ok {
			continue
		}
		name, ok = strings.CutSuffix(name, FileExt)
		if !ok {
			continue
		}
		created, trigger, ok := ParseName(name)
		if !ok {
			continue
		}
		listed[o.Key+"\x00"+o.ETag] = true
		meta, err := m.objectMeta(ctx, st, o)
		if err != nil {
			return nil, err
		}
		out = append(out, BackupInfo{
			Name: name, Size: o.Size, SHA256: meta[metaSHA256], Trigger: trigger, Status: StatusSuccess,
			CreatedAt: created, NezhaVersion: meta[metaVersion], KeyID: meta[metaKeyID],
		})
	}
	// Drop the metadata of objects that are gone (deleted, expired by
	// retention or replaced); the cache used to grow for the process lifetime.
	m.cacheMu.Lock()
	for k := range m.statCache {
		if !listed[k] {
			delete(m.statCache, k)
		}
	}
	m.cacheMu.Unlock()
	return out, nil
}

// objectMeta returns the user metadata of o, reading it with Stat when the
// listing did not include it. Backups are immutable, so it is cached by key
// and ETag.
func (m *Manager) objectMeta(ctx context.Context, st Storage, o ObjectInfo) (map[string]string, error) {
	if o.Metadata[metaSHA256] != "" {
		return o.Metadata, nil
	}
	cacheKey := o.Key + "\x00" + o.ETag
	m.cacheMu.Lock()
	info, ok := m.statCache[cacheKey]
	m.cacheMu.Unlock()
	if ok {
		return info.Metadata, nil
	}
	info, err := st.Stat(ctx, o.Key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	m.cacheMu.Lock()
	m.statCache[cacheKey] = info
	m.cacheMu.Unlock()
	return info.Metadata, nil
}

// applyRetention keeps the newest keep backups of trigger and deletes the rest.
func (m *Manager) applyRetention(ctx context.Context, s Settings, trigger Trigger, keep int) {
	if keep < 1 {
		return
	}
	list, err := m.listStored(ctx, s)
	if err != nil {
		log.Printf("NEZHA>> backup retention: list failed: %v", err)
		return
	}
	var matching []BackupInfo
	for _, b := range list {
		if b.Trigger == trigger {
			matching = append(matching, b)
		}
	}
	slices.SortFunc(matching, func(a, b BackupInfo) int { return strings.Compare(b.Name, a.Name) })
	if len(matching) <= keep {
		return
	}
	st, err := m.storage(s)
	if err != nil {
		return
	}
	for _, b := range matching[keep:] {
		err := st.Delete(ctx, s.S3.Prefix+b.Name+FileExt)
		m.audit.record(auditEntry{Action: "retention_delete", Actor: Actor{System: true}, Name: b.Name, SHA256: b.SHA256, Result: resultOf(err), Error: errString(err)})
		if err != nil {
			log.Printf("NEZHA>> backup retention: delete %s failed: %v", b.Name, err)
		}
	}
	m.invalidateList()
}

// Delete removes the named backups.
func (m *Manager) Delete(ctx context.Context, names []string, actor Actor) error {
	if err := m.checkRestarting(); err != nil {
		return err
	}
	for _, n := range names {
		if !ValidName(n) {
			return ErrInvalidName
		}
	}
	s, _ := m.effective()
	st, err := m.storage(s)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s3store.DefaultTimeout)
	defer cancel()
	defer m.invalidateList()
	for _, n := range names {
		err := st.Delete(ctx, s.S3.Prefix+n+FileExt)
		if errors.Is(err, ErrNotFound) {
			err = nil
		}
		m.audit.record(auditEntry{Action: "delete", Actor: actor, Name: n, Result: resultOf(err), Error: errString(err)})
		if err != nil {
			return err
		}
	}
	m.jobsMu.Lock()
	m.jobs = slices.DeleteFunc(m.jobs, func(j BackupInfo) bool { return j.Status == StatusFailed && slices.Contains(names, j.Name) })
	m.jobsMu.Unlock()
	return nil
}

// Open returns the content of a stored backup for download.
func (m *Manager) Open(ctx context.Context, name string, actor Actor) (io.ReadCloser, ObjectInfo, error) {
	if !ValidName(name) {
		return nil, ObjectInfo{}, ErrInvalidName
	}
	s, _ := m.effective()
	st, err := m.storage(s)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	rc, info, err := st.Get(ctx, s.S3.Prefix+name+FileExt)
	m.audit.record(auditEntry{Action: "download", Actor: actor, Name: name, SHA256: info.Metadata[metaSHA256], Result: resultOf(err), Error: errString(err)})
	return rc, info, err
}

func resultOf(err error) string {
	if err != nil {
		return "failed"
	}
	return "ok"
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
