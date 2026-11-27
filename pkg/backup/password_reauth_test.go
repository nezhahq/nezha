package backup

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Setting or resetting the backup password lets the holder decrypt every
// later backup, so it needs the same re-authentication as a restore.
func TestSetAndResetPasswordRequireReauth(t *testing.T) {
	e := newTestEnv(t)
	denied := errors.New("login password required")
	deny := func() error { return denied }

	_, _, err := e.m.SetPassword(testPassword, deny, admin)
	require.ErrorIs(t, err, denied)
	assert.Nil(t, e.m.View().Encryption, "no key pair without re-authentication")
	_, _, err = e.m.SetPassword(testPassword, nil, admin)
	require.Error(t, err)
	_, _, err = e.m.SetPassword(testPassword, okReauth, admin)
	require.NoError(t, err)
	keyID := e.m.View().Encryption.KeyID

	_, _, err = e.m.ResetPassword("reset password one", ResetConfirmWord, deny, admin)
	require.ErrorIs(t, err, denied)
	assert.Equal(t, keyID, e.m.View().Encryption.KeyID, "the key pair is unchanged")
	_, _, err = e.m.ResetPassword("reset password one", ResetConfirmWord, okReauth, admin)
	require.NoError(t, err)
	assert.NotEqual(t, keyID, e.m.View().Encryption.KeyID)
}

// Wrong login passwords on the password endpoints are rate limited.
func TestPasswordReauthIsRateLimited(t *testing.T) {
	e := newTestEnv(t)
	deny := func() error { return errors.New("wrong login password") }
	for range rateAttempts {
		_, _, err := e.m.SetPassword(testPassword, deny, admin)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrRateLimited)
	}
	_, _, err := e.m.SetPassword(testPassword, okReauth, admin)
	require.ErrorIs(t, err, ErrRateLimited)
}
