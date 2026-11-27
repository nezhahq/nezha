package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// MinPasswordLength is the minimum length, in characters, of a backup password.
const MinPasswordLength = 12

const maxPasswordLength = 1024

// ScryptWorkFactor is the scrypt cost (log2 N) used to wrap the private key
// with the backup password: 18 is age's default (about 256 MiB and a second
// per attempt), which makes every offline password guess expensive. Tests
// lower it.
var ScryptWorkFactor = 18

// maxScryptWorkFactor bounds the cost accepted when unwrapping, so a crafted
// backup header cannot make the dashboard allocate more memory than its own
// backups need.
const maxScryptWorkFactor = 18

var (
	ErrPasswordTooShort   = errors.New("backup password must be at least 12 characters")
	ErrPasswordTooLong    = errors.New("backup password is too long")
	ErrWrongPassword      = errors.New("wrong backup password or recovery key")
	ErrInvalidRecoveryKey = errors.New("invalid recovery key")
	ErrKeyMismatch        = errors.New("the key does not belong to this backup")
)

// KeyMaterial is everything the dashboard keeps about the backup key pair.
// Only the public key is usable without the password: the private key is
// stored wrapped (age scrypt) by the backup password and is never written in
// the clear. Scheduled backups therefore need no password at all.
type KeyMaterial struct {
	Recipient       string    `json:"recipient"`
	WrappedIdentity string    `json:"wrapped_identity"`
	KeyID           string    `json:"key_id"`
	CreatedAt       time.Time `json:"created_at"`
}

// ValidatePassword checks the length policy of a new backup password.
func ValidatePassword(password string) error {
	n := utf8.RuneCountInString(password)
	if n < MinPasswordLength {
		return ErrPasswordTooShort
	}
	if len(password) > maxPasswordLength {
		return ErrPasswordTooLong
	}
	return nil
}

// KeyIDOf returns the short fingerprint of an age recipient string.
func KeyIDOf(recipient string) string {
	sum := sha256.Sum256([]byte(recipient))
	return hex.EncodeToString(sum[:8])
}

// NewKeyMaterial generates a fresh key pair protected by password. The
// returned recovery key is the raw private key: it is shown to the
// administrator once and can replace the password for every backup made
// with this key pair.
func NewKeyMaterial(password string, now time.Time) (KeyMaterial, string, error) {
	if err := ValidatePassword(password); err != nil {
		return KeyMaterial{}, "", err
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return KeyMaterial{}, "", err
	}
	wrapped, err := wrapIdentity(id, password)
	if err != nil {
		return KeyMaterial{}, "", err
	}
	recipient := id.Recipient().String()
	return KeyMaterial{
		Recipient:       recipient,
		WrappedIdentity: wrapped,
		KeyID:           KeyIDOf(recipient),
		CreatedAt:       now.UTC(),
	}, id.String(), nil
}

// Rewrap returns k with the private key wrapped by newPassword instead of
// oldPassword. The key pair itself does not change, so every earlier backup
// stays decryptable with the same recovery key.
func (k KeyMaterial) Rewrap(oldPassword, newPassword string) (KeyMaterial, error) {
	if err := ValidatePassword(newPassword); err != nil {
		return KeyMaterial{}, err
	}
	id, err := UnwrapIdentity(k.WrappedIdentity, oldPassword)
	if err != nil {
		return KeyMaterial{}, err
	}
	if KeyIDOf(id.Recipient().String()) != k.KeyID {
		return KeyMaterial{}, ErrKeyMismatch
	}
	wrapped, err := wrapIdentity(id, newPassword)
	if err != nil {
		return KeyMaterial{}, err
	}
	k.WrappedIdentity = wrapped
	return k, nil
}

func wrapIdentity(id *age.X25519Identity, password string) (string, error) {
	r, err := age.NewScryptRecipient(password)
	if err != nil {
		return "", err
	}
	r.SetWorkFactor(ScryptWorkFactor)
	var buf bytes.Buffer
	aw := armor.NewWriter(&buf)
	w, err := age.Encrypt(aw, r)
	if err != nil {
		return "", err
	}
	if _, err := io.WriteString(w, id.String()); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	if err := aw.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// UnwrapIdentity opens a wrapped private key with password.
func UnwrapIdentity(wrapped, password string) (*age.X25519Identity, error) {
	if password == "" || len(password) > maxPasswordLength {
		return nil, ErrWrongPassword
	}
	si, err := age.NewScryptIdentity(password)
	if err != nil {
		return nil, ErrWrongPassword
	}
	si.SetMaxWorkFactor(maxScryptWorkFactor)
	r, err := age.Decrypt(armor.NewReader(strings.NewReader(wrapped)), si)
	if err != nil {
		return nil, ErrWrongPassword
	}
	raw, err := io.ReadAll(io.LimitReader(r, 256))
	if err != nil {
		return nil, ErrWrongPassword
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, ErrWrongPassword
	}
	return id, nil
}

// ParseRecoveryKey parses a recovery key as shown when the password was set.
func ParseRecoveryKey(s string) (*age.X25519Identity, error) {
	s = strings.TrimSpace(s)
	if len(s) > 256 {
		return nil, ErrInvalidRecoveryKey
	}
	id, err := age.ParseX25519Identity(strings.ToUpper(s))
	if err != nil {
		return nil, ErrInvalidRecoveryKey
	}
	return id, nil
}

// identityKeyID is the key id of the public half of id.
func identityKeyID(id *age.X25519Identity) string {
	return KeyIDOf(id.Recipient().String())
}
