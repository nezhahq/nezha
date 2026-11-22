package controller

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/pkg/backup"
	"github.com/nezhahq/nezha/pkg/backup/s3store"
	"github.com/nezhahq/nezha/pkg/i18n"
	"github.com/nezhahq/nezha/service/singleton"
)

// Every user-facing backup error goes through the gettext catalogue with its
// English text as msgid, so untranslated languages read exactly as before.
func TestBackupErrorsAreLocalized(t *testing.T) {
	old := singleton.Localizer
	singleton.Localizer = i18n.NewLocalizer("en_US", "nezha", "translations", i18n.Translations)
	t.Cleanup(func() { singleton.Localizer = old })

	same := []error{
		backup.ErrNotConfigured, backup.ErrStorageMissing, backup.ErrBusy, backup.ErrRestarting, backup.ErrInvalidName,
		backup.ErrPasswordSet, backup.ErrPasswordNotSet, backup.ErrResetConfirm, backup.ErrSecretRequired,
		backup.ErrInvalidBucketName, backup.ErrRegionTooLong, backup.ErrInvalidPrefix,
		s3store.ErrEndpointNotURL, s3store.ErrEndpointParts, s3store.ErrEndpointScheme,
		backup.ErrPasswordTooShort, backup.ErrPasswordTooLong, backup.ErrWrongPassword, backup.ErrInvalidRecoveryKey,
		backup.ErrKeyMismatch, backup.ErrNoCredential, backup.ErrRateLimited, backup.ErrRestoreToken,
		backup.ErrChecksumMismatch, backup.ErrConfirmWord, backup.ErrAckRequired, backup.ErrNotBackup, backup.ErrBadHeader,
		backup.ErrUnsupported, backup.ErrCorrupt, backup.ErrNotFound, backup.ErrPreRestore,
		errBackupUnavailable, errLoginPasswordNeed, errLoginPasswordWrong, errReauthNeeded, errUploadMultiple, errUploadMissing,
	}
	for _, e := range same {
		got := localizeBackupError(fmt.Errorf("context: %w", e))
		require.Error(t, got)
		assert.Equal(t, e.Error(), got.Error(), "msgid must equal the English text of %v", e)
		assert.False(t, errors.Is(got, e), "%v must be replaced by the catalogue message", e)
	}

	cases := map[error]string{
		&backup.LockedError{Field: "endpoint"}:                                   "endpoint: this setting is managed by an environment variable",
		&s3store.Error{Code: "AccessDenied", Message: "denied"}:                  "storage error AccessDenied: denied",
		&s3store.Error{Message: "timeout"}:                                       "storage error: timeout",
		fmt.Errorf("%w: bad field", backup.ErrInvalidCron):                       "invalid backup schedule: bad field",
		backup.ErrInvalidRetention:                                               fmt.Sprintf("retention must be between 1 and %d", backup.MaxRetention),
		fmt.Errorf("%w: %w", backup.ErrPreRestore, &s3store.Error{Message: "x"}): "pre-restore backup failed, nothing was changed",
	}
	for in, want := range cases {
		assert.Equal(t, want, localizeBackupError(in).Error())
	}
	assert.Nil(t, localizeBackupError(nil))
	assert.Equal(t, errNoop, localizeBackupError(errNoop))
	other := errors.New("unrelated")
	assert.Equal(t, other, localizeBackupError(other))
}

// xgettext (script/i18n.sh, -kErrorT) only extracts literal msgids.
func TestBackupErrorMsgidsAreLiterals(t *testing.T) {
	src, err := os.ReadFile("backup.go")
	require.NoError(t, err)
	calls := regexp.MustCompile(`ErrorT\(([^)]?)`).FindAllStringSubmatch(string(src), -1)
	require.NotEmpty(t, calls)
	for _, c := range calls {
		assert.Equal(t, `"`, c[1], "ErrorT must be called with a string literal")
	}
}
