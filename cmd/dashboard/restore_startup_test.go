package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/pkg/backup"
)

type memStore struct {
	mu   sync.Mutex
	objs map[string][]byte
	meta map[string]map[string]string
}

func (s *memStore) Put(_ context.Context, key string, r io.Reader, _ int64, meta map[string]string) (backup.ObjectInfo, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return backup.ObjectInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[key] = data
	s.meta[key] = meta
	return backup.ObjectInfo{Key: key, Size: int64(len(data)), Metadata: meta}, nil
}

func (s *memStore) Get(_ context.Context, key string) (io.ReadCloser, backup.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objs[key]
	if !ok {
		return nil, backup.ObjectInfo{}, backup.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), backup.ObjectInfo{Key: key, Size: int64(len(data)), Metadata: s.meta[key]}, nil
}

func (s *memStore) Stat(ctx context.Context, key string) (backup.ObjectInfo, error) {
	rc, info, err := s.Get(ctx, key)
	if err == nil {
		rc.Close()
	}
	return info, err
}

func (s *memStore) List(_ context.Context, prefix string) ([]backup.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []backup.ObjectInfo
	for k, v := range s.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, backup.ObjectInfo{Key: k, Size: int64(len(v)), ETag: k})
		}
	}
	return out, nil
}

func (s *memStore) Delete(context.Context, string) error { return nil }

// mock: test-only password.
const mockRestorePassword = "restore startup test password"

func readNames(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer db.Close()
	rows, err := db.Query("SELECT name FROM servers ORDER BY id")
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		out = append(out, n)
	}
	return out
}

// stagedRestore backs up a data directory, changes it and applies the
// backup, leaving a restore that the next start swaps in.
func stagedRestore(t *testing.T) (dir, dbPath, confPath string) {
	t.Helper()
	backup.ScryptWorkFactor = 10
	dir = t.TempDir()
	dbPath, confPath = filepath.Join(dir, "sqlite.db"), filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(confPath, []byte("listen_port: 8008\n"), 0600))
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	for _, stmt := range []string{
		"CREATE TABLE users (id INTEGER PRIMARY KEY, role INTEGER)", "INSERT INTO users VALUES (1, 0)",
		"CREATE TABLE servers (id INTEGER PRIMARY KEY, name TEXT)", "INSERT INTO servers (name) VALUES ('old')",
	} {
		_, err := db.Exec(stmt)
		require.NoError(t, err)
	}
	store := &memStore{objs: map[string][]byte{}, meta: map[string]map[string]string{}}
	m, err := backup.NewManager(backup.Options{
		DataDir: dir, ConfigPath: confPath, Version: "v1.2.3",
		Snapshot: func(ctx context.Context, dst string) error {
			_, err := db.ExecContext(ctx, "VACUUM INTO ?", dst)
			return err
		},
		NewStorage: func(backup.S3Settings) (backup.Storage, error) { return store, nil },
	})
	require.NoError(t, err)
	defer m.Close()
	actor := backup.Actor{UserID: 1, SessionID: "s"}
	s := func(v string) *string { return &v }
	_, err = m.UpdateConfig(backup.ConfigUpdate{S3: &backup.S3Update{Endpoint: s("https://s3.example.com"), Bucket: s("nezha-test"),
		AccessKeyID: s("fake-access-key-for-tests"), SecretAccessKey: s("fake-secret-key-for-tests")}}, actor)
	require.NoError(t, err)
	_, _, err = m.SetPassword(mockRestorePassword, actor)
	require.NoError(t, err)
	job, err := m.StartBackup(backup.TriggerManual, actor)
	require.NoError(t, err)
	var b backup.BackupInfo
	require.Eventually(t, func() bool {
		list, _ := m.List(context.Background())
		for _, x := range list {
			if x.Name == job.Name && x.Status == backup.StatusSuccess {
				b = x
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond)

	_, err = db.Exec("UPDATE servers SET name = 'new'")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(confPath, []byte("listen_port: 9999\n"), 0600))
	res, err := m.Prepare(context.Background(), backup.PrepareRequest{Name: b.Name, SHA256: b.SHA256, Password: mockRestorePassword, Actor: actor})
	require.NoError(t, err)
	require.True(t, res.CanApply)
	require.NoError(t, m.Apply(context.Background(), backup.ApplyRequest{Token: res.RestoreToken, Password: mockRestorePassword,
		Confirm: backup.ConfirmWord(b.Name), Reauth: func() error { return nil }, Actor: actor}))
	require.NoError(t, db.Close())
	return dir, dbPath, confPath
}

// The restored configuration names a port that is in use: startup fails
// after the database was opened, and the previous database and
// configuration are back for the next start.
func TestRestoreRolledBackWhenListenerFails(t *testing.T) {
	dir, dbPath, confPath := stagedRestore(t)
	boot, err := backup.BootRestore(confPath, dbPath)
	require.NoError(t, err)
	require.True(t, boot.Applied())
	assert.Equal(t, []string{"old"}, readNames(t, dbPath), "restored data is in place during startup")

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer busy.Close()
	_, listenErr := openDashboardListener("tcp", busy.Addr().String(), dashboardHTTPListener)
	require.Error(t, listenErr)
	require.ErrorIs(t, finishRestoreStartup(boot, listenErr), listenErr)

	assert.Equal(t, []string{"new"}, readNames(t, dbPath))
	conf, err := os.ReadFile(confPath)
	require.NoError(t, err)
	assert.Equal(t, "listen_port: 9999\n", string(conf))
	m, err := backup.NewManager(backup.Options{DataDir: dir, ConfigPath: confPath})
	require.NoError(t, err)
	defer m.Close()
	r, err := m.RestoreStatus()
	require.NoError(t, err)
	assert.Equal(t, "failed", r.Status)
	assert.True(t, r.RolledBack)

	// The next start runs on the previous data.
	boot, err = backup.BootRestore(confPath, dbPath)
	require.NoError(t, err)
	assert.False(t, boot.Applied())
	assert.Equal(t, []string{"new"}, readNames(t, dbPath))
}

func TestRestoreCommittedOnlyWhenReady(t *testing.T) {
	dir, dbPath, confPath := stagedRestore(t)
	boot, err := backup.BootRestore(confPath, dbPath)
	require.NoError(t, err)
	require.NoError(t, finishRestoreStartup(boot, nil))
	m, err := backup.NewManager(backup.Options{DataDir: dir, ConfigPath: confPath})
	require.NoError(t, err)
	defer m.Close()
	r, err := m.RestoreStatus()
	require.NoError(t, err)
	assert.Equal(t, "success", r.Status)
	assert.Equal(t, []string{"old"}, readNames(t, dbPath))
}

func writeTestCertificate(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600))
	return certPath, keyPath
}

// The HTTPS listener and certificate are obtained before the commit and
// are exactly what the server then serves with.
func TestOpenDashboardHTTPS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeTestCertificate(t, dir)

	l, cert, err := openDashboardHTTPS("127.0.0.1:0", 0, "", "")
	require.NoError(t, err, "HTTPS disabled")
	assert.Nil(t, l)
	assert.Nil(t, cert)

	_, _, err = openDashboardHTTPS("127.0.0.1:0", 1, filepath.Join(dir, "missing.pem"), keyPath)
	require.ErrorContains(t, err, "certificate")

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer busy.Close()
	_, _, err = openDashboardHTTPS(busy.Addr().String(), 1, certPath, keyPath)
	require.Error(t, err)

	l, cert, err = openDashboardHTTPS("127.0.0.1:0", 1, certPath, keyPath)
	require.NoError(t, err)
	server := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }),
		ReadHeaderTimeout: time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{*cert}},
	}
	done := make(chan error, 1)
	go func() { done <- serveDashboardHTTPS(server, l) }()
	defer func() {
		_ = server.Close()
		<-done
	}()
	pool := x509.NewCertPool()
	pemData, err := os.ReadFile(certPath)
	require.NoError(t, err)
	require.True(t, pool.AppendCertsFromPEM(pemData))
	// The certificate file changes after the commit: the server keeps the
	// certificate that was checked.
	require.NoError(t, os.WriteFile(certPath, []byte("garbage"), 0600))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"}}}
	resp, err := client.Get("https://" + l.Addr().String())
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, "ok", string(body))
}

// The restored configuration's HTTPS certificate is missing: startup fails
// before the commit and the previous data and configuration come back.
func TestRestoreRolledBackWhenHTTPSFails(t *testing.T) {
	_, dbPath, confPath := stagedRestore(t)
	boot, err := backup.BootRestore(confPath, dbPath)
	require.NoError(t, err)
	require.True(t, boot.Applied())
	_, _, httpsErr := openDashboardHTTPS("127.0.0.1:0", 1, filepath.Join(t.TempDir(), "missing.pem"), "missing-key.pem")
	require.Error(t, httpsErr)
	require.ErrorIs(t, finishRestoreStartup(boot, httpsErr), httpsErr)
	assert.Equal(t, []string{"new"}, readNames(t, dbPath))
	conf, err := os.ReadFile(confPath)
	require.NoError(t, err)
	assert.Equal(t, "listen_port: 9999\n", string(conf))
}

// The commit cannot be recorded: the dashboard must not start, and the next
// start rolls the restore back instead of serving data that a later start
// would discard.
func TestRestoreCommitFailureStopsStartup(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	dir, dbPath, confPath := stagedRestore(t)
	boot, err := backup.BootRestore(confPath, dbPath)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0500))
	err = finishRestoreStartup(boot, nil)
	require.NoError(t, os.Chmod(dir, 0700))
	require.Error(t, err)

	boot, err = backup.BootRestore(confPath, dbPath)
	require.NoError(t, err)
	assert.False(t, boot.Applied())
	assert.Equal(t, []string{"new"}, readNames(t, dbPath), "the uncommitted restore was rolled back")
}
