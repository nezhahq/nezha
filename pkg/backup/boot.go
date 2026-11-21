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
	data, err := os.ReadFile(statePath) // #nosec G304 -- data directory
	switch {
	case err == nil:
		var st restoreState
		if err := json.Unmarshal(data, &st); err != nil {
			// Never guess: starting on a half-swapped data directory could
			// lose data. The operator has to look at it.
			return nil, fmt.Errorf("restore: unreadable %s, refusing to start: %w", statePath, err)
		}
		b.state = &st
		// That restore failed: make sure it can never be tried again before
		// the plan that describes the rollback goes away.
		if err := removeDurably(pendingDir); err != nil {
			return nil, fmt.Errorf("restore: cannot invalidate the pending restore, refusing to start: %w", err)
		}
		if err := b.rollback(errors.New("the dashboard did not start with the restored data")); err != nil {
			return nil, err
		}
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("restore: cannot read %s, refusing to start: %w", statePath, err)
	}

	data, err = os.ReadFile(filepath.Join(pendingDir, pendingFileName)) // #nosec G304 -- data directory
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
	// Remove the staging when this start is done with it. A real crash runs
	// no deferred calls; simulated crashes (tests) skip it the same way.
	crashed := false
	defer func() {
		if !crashed {
			_ = os.RemoveAll(pendingDir)
		}
	}()
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
	var planErr error
	plan := func(path, rollbackName, staged string) {
		step := swapStep{Path: path, Staged: staged}
		_, err := os.Lstat(path)
		switch {
		case err == nil:
			step.Rollback = filepath.Join(rollbackDir, rollbackName)
		case !os.IsNotExist(err):
			planErr = errors.Join(planErr, err)
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
	if planErr != nil {
		return nil, fmt.Errorf("restore: cannot inspect the current data, refusing to start: %w", planErr)
	}
	if err := writeState(statePath, st); err != nil {
		return nil, err
	}
	b.state = st
	// From here on the plan alone describes this restore: invalidate the
	// pending request first, so that no interruption can make a later start
	// apply it again (the staged files stay where the plan points).
	if err := fault("invalidate pending"); err != nil {
		crashed = true
		return nil, err
	}
	if err := removeDurably(filepath.Join(pendingDir, pendingFileName)); err != nil {
		// Nothing has moved yet. Rolling back now would remove the plan while
		// the request may still be there and could be replayed. Keep the
		// plan and refuse to start: the next start invalidates the request
		// first and only then rolls back.
		crashed = true
		return nil, fmt.Errorf("restore: cannot invalidate the pending restore, refusing to start: %w", err)
	}

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
	if err := swap(); err == nil {
		// The moved entries must be on disk before the plan can ever be
		// removed (by Commit). If that cannot be confirmed, keep the plan and
		// refuse to start; the next start rolls back.
		if err := syncDirs(st.dirs()); err != nil {
			crashed = true
			return nil, fmt.Errorf("restore: cannot persist the swapped files, refusing to start: %w", err)
		}
	} else {
		if errors.Is(err, errSimulatedCrash) {
			crashed = true
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
	_, err := os.Lstat(src)
	if err == nil {
		return moveFile(src, dst)
	}
	if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(dst); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s is missing", src)
		}
		return err
	}
	return nil
}

// removeDurably removes path (a file or a directory tree) and syncs its
// parent directory, so the removal survives a crash.
func removeDurably(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// writeFileDurable replaces path with data (fsynced) and syncs the parent
// directory, so that the new file is on disk once it returns.
func writeFileDurable(path string, data []byte) error {
	if err := writeFileAtomic(path, data); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir flushes the entries of a directory (renames, removals) to disk.
// Errors are reported; only platforms and file systems that do not support
// syncing a directory at all are treated as success (see syncDirectory).
func syncDir(dir string) error {
	if err := fault("sync " + dir); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}

// syncDirs syncs every directory that still exists; a directory that was
// removed (the consumed staging) has no entries left to persist.
func syncDirs(dirs []string) error {
	for _, dir := range dirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// dirs returns every directory whose entries a swap or rollback changes.
func (st *restoreState) dirs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(path string) {
		if path == "" {
			return
		}
		if d := filepath.Dir(path); !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, step := range st.Steps {
		add(step.Path)
		add(step.Rollback)
		add(step.Staged)
	}
	return out
}

func writeState(path string, st *restoreState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFileDurable(path, data)
}

// Commit records a restore whose start succeeded. Call it only once the
// dashboard is ready to serve (database, listeners, TLS). If the plan cannot
// be removed, the dashboard must not start: the next start would otherwise
// roll back over data written in the meantime. The plan stays, so the next
// start rolls the restore back instead.
func (b *Boot) Commit() error {
	if !b.Applied() {
		return nil
	}
	if err := fault("commit"); err != nil {
		return err
	}
	statePath := filepath.Join(b.dataDir, stateFileName)
	// Check first that the directory can be synced at all, while the plan
	// is still untouched: a failure here leaves everything for the next
	// start to roll back.
	if err := syncDir(b.dataDir); err != nil {
		return fmt.Errorf("restore: cannot record the restore as committed: %w", err)
	}
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("restore: cannot record the restore as committed: %w", err)
	}
	if err := syncDir(b.dataDir); err != nil {
		// The removal is not confirmed on disk, but it is visible to this
		// system: a supervisor restart would keep the restored data and skip
		// the rollback. Put the plan back so that the next start, whatever
		// survives, rolls the restore back.
		if werr := writeState(statePath, b.state); werr != nil {
			if _, serr := os.Stat(statePath); serr != nil {
				return fmt.Errorf("restore: cannot record the restore as committed (%v) and cannot restore the plan: %w", err, werr)
			}
		}
		return fmt.Errorf("restore: cannot record the restore as committed: %w", err)
	}
	writeRestoreResult(b.dataDir, RestoreResult{
		Status: "success", Name: b.state.Name, SHA256: b.state.SHA256,
		AppliedAt: b.state.AppliedAt, FinishedAt: time.Now().UTC(),
	})
	log.Printf("NEZHA>> restore: dashboard started with backup %s", b.state.Name)
	b.state = nil
	return nil
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
			if !os.IsNotExist(err) {
				// Unknown state of the kept copy: keep the plan and retry.
				errs = append(errs, err)
			}
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
	// The restored entries must be on disk before the plan is removed.
	if len(errs) == 0 {
		if err := syncDirs(st.dirs()); err != nil {
			if errors.Is(err, errSimulatedCrash) {
				return err
			}
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
	if err := fault("rollback finish"); err != nil {
		return err
	}
	if err := removeDurably(filepath.Join(b.dataDir, stateFileName)); err != nil {
		return fmt.Errorf("restore: rolled back, but cannot remove %s: %w", stateFileName, err)
	}
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
