package backup

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- fake ETag only
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// Keep scrypt cheap in tests; production uses ScryptWorkFactor's default.
	ScryptWorkFactor = 10
	os.Exit(m.Run())
}

// mock: fake S3 credentials, never valid anywhere.
const (
	mockAccessKeyID     = "fake-access-key-for-tests"
	mockSecretAccessKey = "fake-secret-key-for-tests"
	testPassword        = "correct horse battery"
)

type memObject struct {
	data []byte
	meta map[string]string
	mod  time.Time
}

// memStorage is an in-process Storage.
type memStorage struct {
	mu       sync.Mutex
	objects  map[string]memObject
	failPuts int // fail this many Put calls
	puts     int
}

func newMemStorage() *memStorage { return &memStorage{objects: map[string]memObject{}} }

func etag(b []byte) string {
	sum := md5.Sum(b) // #nosec G401 -- fake ETag only
	return hex.EncodeToString(sum[:])
}

func (s *memStorage) Put(_ context.Context, key string, r io.Reader, size int64, meta map[string]string) (ObjectInfo, error) {
	s.mu.Lock()
	s.puts++
	if s.failPuts > 0 {
		s.failPuts--
		s.mu.Unlock()
		return ObjectInfo{}, errors.New("injected upload failure")
	}
	s.mu.Unlock()
	data, err := io.ReadAll(r)
	if err != nil {
		return ObjectInfo{}, err
	}
	if int64(len(data)) != size {
		return ObjectInfo{}, errors.New("size mismatch")
	}
	m := map[string]string{}
	for k, v := range meta {
		m[strings.ToLower(k)] = v
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = memObject{data: data, meta: m, mod: time.Now()}
	return ObjectInfo{Key: key, Size: size, ETag: etag(data), Metadata: m}, nil
}

func (s *memStorage) info(key string, o memObject) ObjectInfo {
	return ObjectInfo{Key: key, Size: int64(len(o.data)), ETag: etag(o.data), LastModified: o.mod, Metadata: o.meta}
}

func (s *memStorage) Get(_ context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return nil, ObjectInfo{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(o.data)), s.info(key, o), nil
}

func (s *memStorage) Stat(_ context.Context, key string) (ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return s.info(key, o), nil
}

// List does not return metadata, like S3 ListObjectsV2.
func (s *memStorage) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ObjectInfo
	for k, o := range s.objects {
		if strings.HasPrefix(k, prefix) && !strings.Contains(strings.TrimPrefix(k, prefix), "/") {
			i := s.info(k, o)
			i.Metadata = nil
			out = append(out, i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Key < out[b].Key })
	return out, nil
}

func (s *memStorage) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func (s *memStorage) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *memStorage) set(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.objects[key]
	o.data = data
	s.objects[key] = o
}

// testEnv is a dashboard data directory with a live database and a manager.
type testEnv struct {
	t        *testing.T
	dir      string
	dbPath   string
	confPath string
	db       *sql.DB
	store    *memStorage
	m        *Manager
	restarts atomic.Int32
	maxBytes int64
	now      time.Time
	nowMu    sync.Mutex
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	e := &testEnv{t: t, dir: dir, dbPath: filepath.Join(dir, "sqlite.db"), confPath: filepath.Join(dir, "config.yaml"), store: newMemStorage(), now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	require.NoError(t, os.WriteFile(e.confPath, []byte("listen_port: 8008\nagent_secret_key: original\n"), 0600))
	db, err := sql.Open("sqlite3", e.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		"CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, role INTEGER)",
		"CREATE TABLE servers (id INTEGER PRIMARY KEY, name TEXT)",
		"CREATE TABLE jwt_sessions (key_id TEXT PRIMARY KEY, user_id INTEGER)",
		"INSERT INTO users (id, username, role) VALUES (1, 'admin', 0)",
		"INSERT INTO servers (name) VALUES ('alpha'), ('beta')",
		"INSERT INTO jwt_sessions VALUES ('session-1', 1)",
	} {
		_, err := db.Exec(stmt)
		require.NoError(t, err)
	}
	e.db = db
	e.m = e.newManager("v1.2.3")
	return e
}

func (e *testEnv) clock() time.Time {
	e.nowMu.Lock()
	defer e.nowMu.Unlock()
	e.now = e.now.Add(time.Second)
	return e.now
}

func (e *testEnv) advance(d time.Duration) {
	e.nowMu.Lock()
	defer e.nowMu.Unlock()
	e.now = e.now.Add(d)
}

func (e *testEnv) newManager(version string) *Manager {
	e.t.Helper()
	m, err := NewManager(Options{
		DataDir:    e.dir,
		ConfigPath: e.confPath,
		Version:    version,
		Snapshot: func(ctx context.Context, dst string) error {
			_, err := e.db.ExecContext(ctx, "VACUUM INTO ?", dst)
			return err
		},
		NewStorage:     func(S3Settings) (Storage, error) { return e.store, nil },
		Restart:        func() { e.restarts.Add(1) },
		Now:            e.clock,
		RetryDelay:     time.Millisecond,
		MaxBackupBytes: e.maxBytes,
	})
	require.NoError(e.t, err)
	e.t.Cleanup(m.Close)
	return m
}

func str(s string) *string { return &s }

// configure sets storage and password and returns the recovery key.
func (e *testEnv) configure() string {
	e.t.Helper()
	_, err := e.m.UpdateConfig(ConfigUpdate{S3: &S3Update{
		Endpoint: str("https://s3.example.com"), Bucket: str("nezha-test"),
		AccessKeyID: str(mockAccessKeyID), SecretAccessKey: str(mockSecretAccessKey),
	}}, Actor{UserID: 1})
	require.NoError(e.t, err)
	recovery, _, err := e.m.SetPassword(testPassword, Actor{UserID: 1})
	require.NoError(e.t, err)
	return recovery
}

// backup runs a backup to completion and returns its list entry.
func (e *testEnv) backup(trigger Trigger) BackupInfo {
	e.t.Helper()
	job, err := e.m.StartBackup(trigger, Actor{UserID: 1})
	require.NoError(e.t, err)
	e.m.op.Lock() // released when the background backup finished
	e.m.op.Unlock()
	list, err := e.m.List(context.Background())
	require.NoError(e.t, err)
	for _, b := range list {
		if b.Name == job.Name {
			return b
		}
	}
	e.t.Fatalf("backup %s not listed", job.Name)
	return BackupInfo{}
}

var admin = Actor{UserID: 1, SessionID: "session-1", IP: "192.0.2.1"}

func okReauth() error { return nil }
