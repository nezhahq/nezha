package backup

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/nezhahq/nezha/pkg/backup/s3store"
	"github.com/nezhahq/nezha/pkg/utils"
)

// SettingsFileName is the file, next to config.yaml, that holds the backup
// settings and key material. It is kept apart from config.yaml so that
// restoring a backup (which replaces config.yaml) never rolls back where
// backups go or which key encrypts them, and so that the dashboard settings
// form can never race with it.
const SettingsFileName = "backup.yaml"

const (
	DefaultCron      = "0 0 3 * * *"
	DefaultRetention = 7
	MaxRetention     = 100
	DefaultRegion    = "auto"
	DefaultPrefix    = "nezha-backup/"
)

// Environment variables that override the S3 settings. A field set through
// the environment cannot be changed from the dashboard. When the secret key
// comes from the environment, endpoint and bucket cannot be changed from the
// dashboard either: otherwise a stolen admin session could point the
// operator's credentials at another server.
const (
	EnvS3Endpoint        = "NZ_BACKUP_S3_ENDPOINT"
	EnvS3Bucket          = "NZ_BACKUP_S3_BUCKET"
	EnvS3Region          = "NZ_BACKUP_S3_REGION"
	EnvS3Prefix          = "NZ_BACKUP_S3_PREFIX"
	EnvS3AccessKeyID     = "NZ_BACKUP_S3_ACCESS_KEY_ID"
	EnvS3SecretAccessKey = "NZ_BACKUP_S3_SECRET_ACCESS_KEY" // #nosec G101 -- environment variable name, not a credential
)

// ScheduleSettings controls scheduled backups.
type ScheduleSettings struct {
	Enabled   bool   `json:"enabled"`
	Cron      string `json:"cron"`
	Retention int    `json:"retention"`
}

// S3Settings is the S3 compatible storage target.
type S3Settings struct {
	Endpoint        string `json:"endpoint,omitempty"`
	Bucket          string `json:"bucket,omitempty"`
	Region          string `json:"region,omitempty"`
	Prefix          string `json:"prefix,omitempty"`
	UsePathStyle    *bool  `json:"use_path_style,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
}

// PathStyle reports whether path-style bucket addressing is used (default true).
func (s S3Settings) PathStyle() bool {
	return s.UsePathStyle == nil || *s.UsePathStyle
}

// Complete reports whether every field needed to connect is set.
func (s S3Settings) Complete() bool {
	return s.Endpoint != "" && s.Bucket != "" && s.AccessKeyID != "" && s.SecretAccessKey != ""
}

func (s S3Settings) storeConfig() s3store.Config {
	return s3store.Config{
		Endpoint:        s.Endpoint,
		Bucket:          s.Bucket,
		Region:          s.Region,
		UsePathStyle:    s.PathStyle(),
		AccessKeyID:     s.AccessKeyID,
		SecretAccessKey: s.SecretAccessKey,
	}
}

// Settings is the content of backup.yaml. It holds secrets (S3 secret key,
// MAC key) and must never be serialized into an API response; handlers build
// their responses from View.
type Settings struct {
	// Format is the version of this file's structure (settingsFormat). A
	// file without it predates the field and is format 1.
	Format     int              `json:"format"`
	Schedule   ScheduleSettings `json:"schedule"`
	S3         S3Settings       `json:"s3"`
	Encryption *KeyMaterial     `json:"encryption,omitempty"`
	// AuthKey (hex) authenticates the manifests of backups made by this
	// dashboard. It survives password changes and key resets.
	AuthKey string `json:"auth_key,omitempty"`
	// KnownKeyIDs lists every key pair this dashboard created (current and
	// earlier ones). A backup encrypted to one of them must carry a valid MAC.
	KnownKeyIDs []string `json:"known_key_ids,omitempty"`
}

// ownKeyIDs returns the key ids of every key pair made by this dashboard.
func (s *Settings) ownKeyIDs() []string {
	ids := append([]string(nil), s.KnownKeyIDs...)
	if s.Encryption != nil {
		ids = append(ids, s.Encryption.KeyID)
	}
	return ids
}

func (s *Settings) applyDefaults() {
	if s.Schedule.Cron == "" {
		s.Schedule.Cron = DefaultCron
	}
	if s.Schedule.Retention <= 0 {
		s.Schedule.Retention = DefaultRetention
	}
	if s.S3.Region == "" {
		s.S3.Region = DefaultRegion
	}
	if s.S3.Prefix == "" {
		s.S3.Prefix = DefaultPrefix
	}
}

func (s *Settings) clone() Settings {
	c := *s
	if s.Encryption != nil {
		k := *s.Encryption
		c.Encryption = &k
	}
	c.KnownKeyIDs = append([]string(nil), s.KnownKeyIDs...)
	if s.S3.UsePathStyle != nil {
		v := *s.S3.UsePathStyle
		c.S3.UsePathStyle = &v
	}
	return c
}

func (s *Settings) authKey() []byte {
	b, err := hex.DecodeString(s.AuthKey)
	if err != nil {
		return nil
	}
	return b
}

func (s *Settings) ensureAuthKey() error {
	if len(s.authKey()) == 32 {
		return nil
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	s.AuthKey = hex.EncodeToString(b[:])
	return nil
}

// ErrInvalidPrefix rejects an object key prefix.
var ErrInvalidPrefix = errors.New("prefix may only contain letters, digits, '/', '_' and '-', must not start with '/' or contain '//'")

var prefixRe = regexp.MustCompile(`^[a-zA-Z0-9/_-]*$`)

// NormalizePrefix validates an object key prefix and makes it end with "/".
func NormalizePrefix(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return DefaultPrefix, nil
	}
	if len(p) > 200 || !prefixRe.MatchString(p) || strings.HasPrefix(p, "/") || strings.Contains(p, "//") {
		return "", ErrInvalidPrefix
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p, nil
}

// envOverrides reads the S3 environment overrides. The returned map names
// the overridden fields (by their JSON name).
func envOverrides(s *S3Settings) map[string]bool {
	locked := map[string]bool{}
	set := func(env, field string, dst *string) {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			*dst = v
			locked[field] = true
		}
	}
	set(EnvS3Endpoint, "endpoint", &s.Endpoint)
	set(EnvS3Bucket, "bucket", &s.Bucket)
	set(EnvS3Region, "region", &s.Region)
	set(EnvS3Prefix, "prefix", &s.Prefix)
	set(EnvS3AccessKeyID, "access_key_id", &s.AccessKeyID)
	set(EnvS3SecretAccessKey, "secret_access_key", &s.SecretAccessKey)
	if locked["secret_access_key"] {
		locked["endpoint"] = true
		locked["bucket"] = true
	}
	return locked
}

// settingsFormat is the structure version of backup.yaml written by this
// build. Change it whenever fields are renamed, removed or reinterpreted.
const settingsFormat = 1

// ErrSettingsFormat means backup.yaml was written by a newer dashboard. The
// backup feature stays off and the file is left untouched, so that saving
// settings cannot silently drop what that version stored.
var ErrSettingsFormat = errors.New("backup settings were written by a newer dashboard version")

func loadSettings(path string) (Settings, error) {
	var s Settings
	data, err := os.ReadFile(path) // #nosec G304 -- operator-configured data directory
	if err != nil && !os.IsNotExist(err) {
		return s, err
	}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &s); err != nil {
			return s, err
		}
	}
	if s.Format > settingsFormat {
		return s, fmt.Errorf("%w (format %d, this version reads %d)", ErrSettingsFormat, s.Format, settingsFormat)
	}
	s.Format = settingsFormat
	return s, nil
}

func saveSettings(path string, s Settings) error {
	s.Format = settingsFormat
	data, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// writeFileAtomic replaces path with data through a 0600 temporary file in
// the same directory.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	_ = utils.RestrictFileToOwner(path)
	return nil
}
