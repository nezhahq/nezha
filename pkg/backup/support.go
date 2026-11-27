package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
)

// Actor identifies who asked for an operation, for audit and for binding a
// staged restore to one login session.
type Actor struct {
	UserID    uint64 `json:"user_id,omitempty"`
	Username  string `json:"username,omitempty"`
	SessionID string `json:"-"`
	IP        string `json:"ip,omitempty"`
	System    bool   `json:"system,omitempty"`
}

type hashingWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func newHashingWriter(w io.Writer) *hashingWriter {
	return &hashingWriter{w: w, h: sha256.New()}
}

func (hw *hashingWriter) Write(p []byte) (int, error) {
	n, err := hw.w.Write(p)
	hw.h.Write(p[:n])
	hw.n += int64(n)
	return n, err
}

func (hw *hashingWriter) sum() string {
	return hex.EncodeToString(hw.h.Sum(nil))
}

// ---- audit ----

// The audit log is a file in the data directory, not a database table: a
// restore replaces the database and would erase the record of the restore.
const auditMaxBytes = 1 << 20

type auditEntry struct {
	Time   time.Time `json:"time"`
	Action string    `json:"action"`
	Actor  Actor     `json:"actor"`
	Name   string    `json:"name,omitempty"`
	SHA256 string    `json:"sha256,omitempty"`
	KeyID  string    `json:"key_id,omitempty"`
	Result string    `json:"result"`
	Error  string    `json:"error,omitempty"`
}

type auditLog struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
}

func newAuditLog(path string, now func() time.Time) *auditLog {
	return &auditLog{path: path, now: now}
}

func (a *auditLog) record(e auditEntry) {
	e.Time = a.now().UTC()
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	log.Printf("NEZHA>> backup audit: %s", data)
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, err := os.Stat(a.path); err == nil && st.Size() > auditMaxBytes {
		_ = os.Rename(a.path, a.path+".1")
	}
	f, err := os.OpenFile(a.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600) // #nosec G304 -- data directory
	if err != nil {
		log.Printf("NEZHA>> backup audit: %v", err)
		return
	}
	_, _ = f.Write(append(data, '\n'))
	_ = f.Close()
}

// ---- rate limiting ----

// ErrRateLimited is returned when an administrator made too many restore or
// password attempts.
var ErrRateLimited = errors.New("too many attempts, try again later")

const (
	rateWindow   = 15 * time.Minute
	rateAttempts = 5
)

type rateLimiter struct {
	mu   sync.Mutex
	now  func() time.Time
	hits map[string][]time.Time
}

func newRateLimiter(now func() time.Time) *rateLimiter {
	return &rateLimiter{now: now, hits: map[string][]time.Time{}}
}

// allow records an attempt of action by user and fails once more than
// rateAttempts were made within rateWindow. Every attempt counts, successful
// or not, so a wrong password cannot be retried quickly.
func (l *rateLimiter) allow(userID uint64, action string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	key := action + "\x00" + strconv.FormatUint(userID, 10)
	// Forget users whose attempts all left the window; otherwise every user
	// and action that ever tried stayed in the map for the process lifetime.
	for k, hits := range l.hits {
		if k != key && (len(hits) == 0 || now.Sub(hits[len(hits)-1]) >= rateWindow) {
			delete(l.hits, k)
		}
	}
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < rateWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rateAttempts {
		l.hits[key] = kept
		return ErrRateLimited
	}
	l.hits[key] = append(kept, now)
	return nil
}
