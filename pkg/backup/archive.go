package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Entries of the encrypted payload (a tar.gz). Extraction accepts exactly
// these names as regular files and nothing else.
const (
	FileManifest    = "manifest.json"
	FileManifestMAC = "manifest.mac"
	FileDatabase    = "sqlite.db"
	FileConfig      = "config.yaml"
)

// FileEntry describes one data file of a backup.
type FileEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is the encrypted description of a backup. It repeats the header
// fields so that a tampered header is detected after decryption.
type Manifest struct {
	Format       int       `json:"format"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	Trigger      Trigger   `json:"trigger"`
	NezhaVersion string    `json:"nezha_version"`
	KeyID        string    `json:"key_id"`
	// AuthKeyID identifies the dashboard key that made manifest.mac. A
	// dashboard that holds this key must find a valid MAC.
	AuthKeyID string           `json:"auth_key_id,omitempty"`
	Files     []FileEntry      `json:"files"`
	Tables    map[string]int64 `json:"tables"`
}

func (m *Manifest) file(name string) (FileEntry, bool) {
	for _, f := range m.Files {
		if f.Name == name {
			return f, true
		}
	}
	return FileEntry{}, false
}

func (m *Manifest) matchesHeader(h Header) bool {
	return m.Format == h.Format && m.Name == h.Name && m.CreatedAt.Equal(h.CreatedAt) &&
		m.Trigger == h.Trigger && m.NezhaVersion == h.NezhaVersion && m.KeyID == h.KeyID
}

// authKeyID is the public fingerprint of a MAC key. It is an HMAC keyed by
// authKey: authKey is 32 random bytes (ensureAuthKey), never a password, but
// a plain SHA256 over a "key" is flagged by CodeQL as weak password hashing
// (go/weak-sensitive-data-hashing), so keep it keyed.
func authKeyID(authKey []byte) string {
	if len(authKey) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, authKey)
	mac.Write([]byte("nezha-backup-auth-key-id"))
	return hex.EncodeToString(mac.Sum(nil)[:8])
}

func manifestMAC(authKey, raw []byte) string {
	mac := hmac.New(sha256.New, authKey)
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

// writePayload writes the tar.gz payload: the data files named in sources
// (entry name -> path on disk), then the manifest and its MAC. m.Files is
// filled in while the files are copied.
//
// It returns the total size of all entries, which is what extractPayload
// limits on restore; the caller must apply the same limit.
func writePayload(w io.Writer, m *Manifest, sources map[string]string, authKey []byte) (int64, error) {
	var total int64
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	m.Files = m.Files[:0]
	for _, name := range []string{FileDatabase, FileConfig} {
		path, ok := sources[name]
		if !ok {
			continue
		}
		entry, err := addFile(tw, name, path, m.CreatedAt)
		if err != nil {
			return 0, err
		}
		total += entry.Size
		m.Files = append(m.Files, entry)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	if err := addBytes(tw, FileManifest, raw, m.CreatedAt); err != nil {
		return 0, err
	}
	total += int64(len(raw))
	if len(authKey) > 0 {
		mac := manifestMAC(authKey, raw)
		if err := addBytes(tw, FileManifestMAC, []byte(mac), m.CreatedAt); err != nil {
			return 0, err
		}
		total += int64(len(mac))
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	return total, gz.Close()
}

func addFile(tw *tar.Writer, name, path string, mtime time.Time) (FileEntry, error) {
	f, err := os.Open(path) // #nosec G304 -- paths are the dashboard's own snapshot and config file
	if err != nil {
		return FileEntry{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return FileEntry{}, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: st.Size(), ModTime: mtime, Typeflag: tar.TypeReg}); err != nil {
		return FileEntry{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tw, h), f)
	if err != nil {
		return FileEntry{}, err
	}
	if n != st.Size() {
		return FileEntry{}, fmt.Errorf("%s changed while being archived", name)
	}
	return FileEntry{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func addBytes(tw *tar.Writer, name string, data []byte, mtime time.Time) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), ModTime: mtime, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// extracted is the result of a successful payload extraction.
type extracted struct {
	Manifest    Manifest
	RawManifest []byte
	MAC         string
	Paths       map[string]string // entry name -> extracted path
}

// extractPayload unpacks a decrypted payload into dir, which must be an
// empty directory owned by the dashboard. Only the known entry names are
// accepted, each at most once, as regular files; the total size is capped by
// maxBytes. Every data file must match the manifest exactly.
func extractPayload(r io.Reader, dir string, maxBytes int64) (*extracted, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, ErrCorrupt
	}
	tr := tar.NewReader(gz)
	out := &extracted{Paths: map[string]string{}}
	seen := map[string]bool{}
	hashes := map[string]FileEntry{}
	remaining := maxBytes
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, ErrCorrupt
		}
		name := hdr.Name
		if hdr.Typeflag != tar.TypeReg || seen[name] {
			return nil, fmt.Errorf("%w: unexpected entry %q", ErrCorrupt, name)
		}
		seen[name] = true
		if hdr.Size < 0 || hdr.Size > remaining {
			return nil, fmt.Errorf("%w: payload exceeds the size limit", ErrCorrupt)
		}
		remaining -= hdr.Size
		switch name {
		case FileManifest:
			raw, err := io.ReadAll(io.LimitReader(tr, 1<<20))
			if err != nil || int64(len(raw)) != hdr.Size {
				return nil, ErrCorrupt
			}
			out.RawManifest = raw
		case FileManifestMAC:
			raw, err := io.ReadAll(io.LimitReader(tr, 128))
			if err != nil || int64(len(raw)) != hdr.Size {
				return nil, ErrCorrupt
			}
			out.MAC = string(raw)
		case FileDatabase, FileConfig:
			// The path is built from the package constant, never from the
			// name in the archive: only these two fixed names are written.
			fixed := FileDatabase
			if name == FileConfig {
				fixed = FileConfig
			}
			path := filepath.Join(dir, fixed)
			entry, err := writeExclusive(path, tr, hdr.Size)
			if err != nil {
				return nil, err
			}
			entry.Name = fixed
			hashes[fixed] = entry
			out.Paths[fixed] = path
		default:
			return nil, fmt.Errorf("%w: unexpected entry %q", ErrCorrupt, name)
		}
	}
	if out.RawManifest == nil {
		return nil, fmt.Errorf("%w: missing manifest", ErrCorrupt)
	}
	if err := json.Unmarshal(out.RawManifest, &out.Manifest); err != nil {
		return nil, ErrCorrupt
	}
	if _, ok := out.Manifest.file(FileDatabase); !ok {
		return nil, fmt.Errorf("%w: missing database", ErrCorrupt)
	}
	if len(out.Manifest.Files) != len(hashes) {
		return nil, fmt.Errorf("%w: file list does not match the manifest", ErrCorrupt)
	}
	for _, f := range out.Manifest.Files {
		got, ok := hashes[f.Name]
		if !ok || got.Size != f.Size || got.SHA256 != f.SHA256 {
			return nil, fmt.Errorf("%w: %s does not match the manifest", ErrCorrupt, f.Name)
		}
	}
	return out, nil
}

// writeExclusive creates path (which must not exist) with mode 0600 and
// copies exactly size bytes from r into it.
func writeExclusive(path string, r io.Reader, size int64) (FileEntry, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) // #nosec G304 -- path is a fixed name inside a dashboard-created directory
	if err != nil {
		return FileEntry{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return FileEntry{}, ErrCorrupt
	}
	if n != size {
		return FileEntry{}, ErrCorrupt
	}
	return FileEntry{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// fileSHA256 hashes a file on disk.
func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path) // #nosec G304 -- dashboard-owned staging path
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
