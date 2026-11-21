package backup

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned by a Storage when an object does not exist.
var ErrNotFound = errors.New("backup not found")

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
	// Metadata holds the user metadata written with Put (lower-case keys).
	// It is informational only; nothing trusts it for a restore.
	Metadata map[string]string
}

// Storage is where encrypted backups live. Keys are full object keys
// (prefix included). The first implementation is S3 compatible storage;
// other targets only need these five operations.
type Storage interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, metadata map[string]string) (ObjectInfo, error)
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}

// Object metadata keys written with every backup.
const (
	metaSHA256  = "sha256"
	metaVersion = "nezha-version"
	metaTrigger = "trigger"
	metaKeyID   = "key-id"
)
