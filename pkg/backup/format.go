package backup

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"filippo.io/age"
)

// A .nzbak file is
//
//	"NZBAK1\n" | uint32 big-endian header length | header JSON | age ciphertext
//
// The header is the only part readable without a key: it lets the dashboard
// list a backup, pick the matching key and unwrap the private key with the
// password. Everything else (manifest, database, config) is inside the age
// payload, which is authenticated encryption (X25519 + ChaCha20-Poly1305
// STREAM), so tampering or truncation fails decryption. The header itself is
// not authenticated; the encrypted manifest repeats its fields and the
// restore path compares them.
const (
	fileMagic     = "NZBAK1\n"
	formatVersion = 1
	maxHeaderSize = 64 << 10
)

var (
	ErrNotBackup   = errors.New("not a nezha backup file")
	ErrBadHeader   = errors.New("malformed backup header")
	ErrUnsupported = errors.New("unsupported backup format version")
	ErrCorrupt     = errors.New("backup file is corrupt or has been modified")
)

var keyIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Header is the plaintext part of a backup file.
type Header struct {
	Format          int       `json:"format"`
	Name            string    `json:"name"`
	CreatedAt       time.Time `json:"created_at"`
	Trigger         Trigger   `json:"trigger"`
	NezhaVersion    string    `json:"nezha_version"`
	KeyID           string    `json:"key_id"`
	WrappedIdentity string    `json:"wrapped_identity"`
}

func (h Header) validate() error {
	if h.Format != formatVersion {
		return ErrUnsupported
	}
	t, trigger, ok := ParseName(h.Name)
	if !ok || trigger != h.Trigger || !t.Equal(h.CreatedAt.UTC().Truncate(time.Second)) {
		return ErrBadHeader
	}
	if !keyIDRe.MatchString(h.KeyID) || len(h.NezhaVersion) > 64 {
		return ErrBadHeader
	}
	if !strings.HasPrefix(h.WrappedIdentity, "-----BEGIN AGE ENCRYPTED FILE-----") || len(h.WrappedIdentity) > 4096 {
		return ErrBadHeader
	}
	return nil
}

// writeHeader writes the magic and the header.
func writeHeader(w io.Writer, h Header) error {
	h.Format = formatVersion
	h.CreatedAt = h.CreatedAt.UTC().Truncate(time.Second)
	if err := h.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(data)))
	if _, err := io.WriteString(w, fileMagic); err != nil {
		return err
	}
	if _, err := w.Write(size[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// ReadHeader reads and validates the header of a backup file, leaving r
// positioned at the start of the encrypted payload.
func ReadHeader(r io.Reader) (Header, error) {
	magic := make([]byte, len(fileMagic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != fileMagic {
		return Header{}, ErrNotBackup
	}
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return Header{}, ErrBadHeader
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > maxHeaderSize {
		return Header{}, ErrBadHeader
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return Header{}, ErrBadHeader
	}
	var h Header
	if err := json.Unmarshal(data, &h); err != nil {
		return Header{}, ErrBadHeader
	}
	if err := h.validate(); err != nil {
		return Header{}, err
	}
	return h, nil
}

// encryptTo writes the header to w and returns a writer that encrypts the
// payload to recipient. Close must be called to flush the last chunk.
func encryptTo(w io.Writer, h Header, recipient string) (io.WriteCloser, error) {
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return nil, fmt.Errorf("invalid backup public key: %w", err)
	}
	if KeyIDOf(recipient) != h.KeyID {
		return nil, ErrKeyMismatch
	}
	if err := writeHeader(w, h); err != nil {
		return nil, err
	}
	return age.Encrypt(w, r)
}

// decryptPayload returns the plaintext payload of a backup file whose header
// has already been read from r.
func decryptPayload(r io.Reader, id age.Identity) (io.Reader, error) {
	pr, err := age.Decrypt(bufio.NewReader(r), id)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, ErrKeyMismatch
		}
		return nil, ErrCorrupt
	}
	return pr, nil
}
