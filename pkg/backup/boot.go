package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

// pendingRestore is written by Apply and consumed by BootRestore.
type pendingRestore struct {
	Dir        string            `json:"dir"`
	Files      map[string]string `json:"files"`
	Name       string            `json:"name"`
	SHA256     string            `json:"sha256"`
	UserID     uint64            `json:"user_id"`
	PreparedAt time.Time         `json:"prepared_at"`
}

// RestoreResult is the outcome of the last restore, kept in the data
// directory so that it survives the restore itself.
type RestoreResult struct {
	Status     string    `json:"status"` // success | failed
	Name       string    `json:"name"`
	SHA256     string    `json:"sha256"`
	AppliedAt  time.Time `json:"applied_at"`
	FinishedAt time.Time `json:"finished_at"`
	RolledBack bool      `json:"rolled_back"`
	Error      string    `json:"error,omitempty"`
}

// swapStep is one live path touched by a restore. Rollback is where the
// previous file is kept ("" if the path did not exist before the restore);
// Staged is the verified file installed at Path ("" for SQLite sidecars).
type swapStep struct {
	Path     string `json:"path"`
	Rollback string `json:"rollback,omitempty"`
	Staged   string `json:"staged,omitempty"`
}

// restoreState is the swap plan. It is written before the first file is
// moved, so that a start that dies at any point (during the swap, during a
// rollback, or before Commit) is rolled back by the next start. Every step
// is idempotent and can be repeated after an interruption.
type restoreState struct {
	Name      string     `json:"name"`
	SHA256    string     `json:"sha256"`
	AppliedAt time.Time  `json:"applied_at"`
	Steps     []swapStep `json:"steps"`
}

// errSimulatedCrash is returned by bootFault in tests to stop BootRestore
// as if the process died at that point.
var errSimulatedCrash = errors.New("simulated crash")

// bootFault, when set (tests only), is called before every file operation of
// a swap or rollback; returning errSimulatedCrash aborts like a crash.
var bootFault func(op string) error

func fault(op string) error {
	if bootFault != nil {
		return bootFault(op)
	}
	return nil
}

// Boot is a restore applied during this start.
type Boot struct {
	dataDir string
	state   *restoreState
}

// Applied reports whether this start swapped in a restore.
func (b *Boot) Applied() bool { return b != nil && b.state != nil }

func readRestoreResult(dataDir string) (*RestoreResult, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, resultFileName)) // #nosec G304 -- data directory
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r RestoreResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func writeRestoreResult(dataDir string, r RestoreResult) {
	data, err := json.Marshal(r)
	if err == nil {
		err = writeFileAtomic(filepath.Join(dataDir, resultFileName), data)
	}
	if err != nil {
		log.Printf("NEZHA>> restore: cannot record the result: %v", err)
	}
}

// BootRestore runs before the configuration and the database are opened. It
// rolls back a restore whose start never completed, and swaps in a staged
// restore if Apply recorded one. dbDSN is the -db flag value.
func BootRestore(configPath, dbDSN string) (*Boot, error) {
	dbPath := SQLiteFilePath(dbDSN)
	if dbPath == "" {
		return nil, nil
	}
	dataDir := filepath.Dir(dbPath)
	b := &Boot{dataDir: dataDir}
	statePath := filepath.Join(dataDir, stateFileName)

	pendingDir := filepath.Join(dataDir, pendingDirName)
	if data, err := os.ReadFile(statePath); err == nil { // #nosec G304 -- data directory
		var st restoreState
		if err := json.Unmarshal(data, &st); err != nil {
			// Never guess: starting on a half-swapped data directory could
			// lose data. The operator has to look at it.
			return nil, fmt.Errorf("restore: unreadable %s, refusing to start: %w", statePath, err)
		}
		b.state = &st
		if err := b.rollback(errors.New("the dashboard did not start with the restored data")); err != nil {
			return nil, err
		}
		// That restore failed; do not try it again.
		_ = os.RemoveAll(pendingDir)
	}

	data, err := os.ReadFile(filepath.Join(pendingDir, pendingFileName)) // #nosec G304 -- data directory
	if os.IsNotExist(err) {
		// No restore pending: drop staging leftovers and the copy of the data
		// a previous successful restore replaced.
		_ = os.RemoveAll(pendingDir)
		if b.state == nil {
			_ = os.RemoveAll(filepath.Join(dataDir, rollbackDirName))
		}
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(pendingDir)
	var p pendingRestore
	result := RestoreResult{Status: "failed", AppliedAt: time.Now().UTC()}
	if err := json.Unmarshal(data, &p); err != nil || filepath.Base(p.Dir) != p.Dir || p.Dir == "." || p.Dir == ".." {
		result.Error = "the pending restore record is invalid"
		result.FinishedAt = time.Now().UTC()
		writeRestoreResult(dataDir, result)
		return b, nil
	}
	result.Name, result.SHA256 = p.Name, p.SHA256
	staged := filepath.Join(pendingDir, p.Dir, stageFilesDir)
	targets := map[string]string{FileDatabase: dbPath}
	if configPath != "" {
		targets[FileConfig] = configPath
	}
	if _, ok := p.Files[FileDatabase]; !ok {
		result.Error = "the pending restore has no database"
		result.FinishedAt = time.Now().UTC()
		writeRestoreResult(dataDir, result)
		return b, nil
	}
	for name, want := range p.Files {
		if _, ok := targets[name]; !ok {
			result.Error = "the pending restore contains an unexpected file"
			result.FinishedAt = time.Now().UTC()
			writeRestoreResult(dataDir, result)
			return b, nil
		}
		got, _, err := fileSHA256(filepath.Join(staged, name))
		if err != nil || got != want {
			result.Error = "the staged files changed after they were verified"
			result.FinishedAt = time.Now().UTC()
			writeRestoreResult(dataDir, result)
			return b, nil
		}
	}

	rollbackDir := filepath.Join(dataDir, rollbackDirName)
	_ = os.RemoveAll(rollbackDir)
	if err := os.MkdirAll(rollbackDir, 0700); err != nil {
		return nil, err
	}
	// Plan every step and persist the plan before anything is moved. The
	// -journal / -wal / -shm files belong to the old database: left in
	// place, SQLite could replay them onto the restored one; moved with it,
	// the old database keeps every committed transaction.
	st := &restoreState{Name: p.Name, SHA256: p.SHA256, AppliedAt: result.AppliedAt}
	plan := func(path, rollbackName, staged string) {
		step := swapStep{Path: path, Staged: staged}
		if _, err := os.Lstat(path); err == nil {
			step.Rollback = filepath.Join(rollbackDir, rollbackName)
		}
		st.Steps = append(st.Steps, step)
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		plan(dbPath+suffix, FileDatabase+suffix, "")
	}
	plan(dbPath, FileDatabase, filepath.Join(staged, FileDatabase))
	if _, ok := p.Files[FileConfig]; ok {
		plan(configPath, FileConfig, filepath.Join(staged, FileConfig))
	}
	if err := writeState(statePath, st); err != nil {
		return nil, err
	}
	b.state = st

	swap := func() error {
		for _, step := range st.Steps {
			if step.Rollback != "" {
				if err := fault("aside " + step.Path); err != nil {
					return err
				}
				if err := moveOnce(step.Path, step.Rollback); err != nil {
					return err
				}
			}
		}
		for _, step := range st.Steps {
			if step.Staged != "" {
				if err := fault("install " + step.Path); err != nil {
					return err
				}
				if err := moveOnce(step.Staged, step.Path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := swap(); err != nil {
		if errors.Is(err, errSimulatedCrash) {
			return nil, err
		}
		if rerr := b.rollback(fmt.Errorf("swapping in the restored files failed: %w", err)); rerr != nil {
			return nil, rerr
		}
		return b, nil
	}
	_ = os.RemoveAll(pendingDir)
	log.Printf("NEZHA>> restore: swapped in backup %s; previous data kept in %s until the next successful start", p.Name, rollbackDir)
	return b, nil
}

// moveOnce moves src to dst unless that already happened (src gone, dst
// present), so a swap step can be repeated after an interruption.
func moveOnce(src, dst string) error {
	if _, err := os.Lstat(src); os.IsNotExist(err) {
		if _, err := os.Lstat(dst); err == nil {
			return nil
		}
		return fmt.Errorf("%s is missing", src)
	}
	return moveFile(src, dst)
}

func writeState(path string, st *restoreState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// Commit records a restore whose start succeeded. Call it only once the
// dashboard is ready to serve (database, listeners, TLS).
func (b *Boot) Commit() {
	if !b.Applied() {
		return
	}
	writeRestoreResult(b.dataDir, RestoreResult{
		Status: "success", Name: b.state.Name, SHA256: b.state.SHA256,
		AppliedAt: b.state.AppliedAt, FinishedAt: time.Now().UTC(),
	})
	_ = os.Remove(filepath.Join(b.dataDir, stateFileName))
	log.Printf("NEZHA>> restore: dashboard started with backup %s", b.state.Name)
	b.state = nil
}

// Rollback puts the previous files back after a failed start. If it cannot
// finish, the plan stays on disk and the next start tries again.
func (b *Boot) Rollback(cause error) {
	if !b.Applied() {
		return
	}
	_ = b.rollback(cause)
}

// rollback undoes the plan in reverse order. It is idempotent:
//   - a path that had a previous file: if the kept copy is in the rollback
//     directory, whatever sits at the path now came from the restore and is
//     replaced by it; otherwise the previous file was never moved (or is
//     already back) and is left alone;
//   - a path that did not exist before: anything there came from the
//     restore (including journal files the restored database created) and
//     is removed.
//
// The plan and the kept copies are removed only after every step succeeded.
func (b *Boot) rollback(cause error) error {
	st := b.state
	var errs []error
	for i := len(st.Steps) - 1; i >= 0; i-- {
		step := st.Steps[i]
		if err := fault("rollback " + step.Path); err != nil {
			if errors.Is(err, errSimulatedCrash) {
				return err
			}
			errs = append(errs, err)
			continue
		}
		if step.Rollback == "" {
			if err := os.Remove(step.Path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		if _, err := os.Lstat(step.Rollback); err != nil {
			continue
		}
		if err := os.Remove(step.Path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := moveFile(step.Rollback, step.Path); err != nil {
			errs = append(errs, err)
		}
	}
	result := RestoreResult{
		Status: "failed", Name: st.Name, SHA256: st.SHA256, AppliedAt: st.AppliedAt,
		FinishedAt: time.Now().UTC(), RolledBack: len(errs) == 0, Error: cause.Error(),
	}
	if len(errs) > 0 {
		result.Error = fmt.Sprintf("%v; rolling back failed: %v (previous data is in %s; the next start retries)", cause, errors.Join(errs...), filepath.Join(b.dataDir, rollbackDirName))
	}
	writeRestoreResult(b.dataDir, result)
	log.Printf("NEZHA>> restore: %s", result.Error)
	if len(errs) > 0 {
		return errors.New(result.Error)
	}
	_ = os.Remove(filepath.Join(b.dataDir, stateFileName))
	_ = os.RemoveAll(filepath.Join(b.dataDir, rollbackDirName))
	b.state = nil
	return nil
}

// moveFile renames src to dst, copying when they are on different file systems.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src) // #nosec G304 -- dashboard data files
	if err != nil {
		return err
	}
	defer in.Close()
	// Copy to a temporary name next to dst and rename it into place, so an
	// interrupted copy never leaves a truncated file at dst.
	tmp := dst + ".nezha-tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600) // #nosec G304 -- dashboard data files
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	in.Close()
	return os.Remove(src)
}
