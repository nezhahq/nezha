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

type movedFile struct {
	Original string `json:"original"`
	Rollback string `json:"rollback"`
	Restored bool   `json:"restored"` // a restored file now sits at Original
}

// restoreState is written right after the files are swapped. If the process
// dies before Commit, the next start finds it and puts the old files back.
type restoreState struct {
	DBPath    string      `json:"db_path"`
	Name      string      `json:"name"`
	SHA256    string      `json:"sha256"`
	AppliedAt time.Time   `json:"applied_at"`
	Moved     []movedFile `json:"moved"`
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

	if data, err := os.ReadFile(statePath); err == nil { // #nosec G304 -- data directory
		var st restoreState
		if json.Unmarshal(data, &st) == nil {
			b.state = &st
			b.rollback(errors.New("the dashboard did not start with the restored data"))
		} else {
			_ = os.Remove(statePath)
		}
	}

	pendingDir := filepath.Join(dataDir, pendingDirName)
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
	st := &restoreState{DBPath: dbPath, Name: p.Name, SHA256: p.SHA256, AppliedAt: result.AppliedAt}
	b.state = st
	// Move the current files aside. The -journal / -wal / -shm files belong
	// to the old database: left in place, SQLite could replay them onto the
	// restored one and corrupt it.
	move := func(original, name string) error {
		if _, err := os.Lstat(original); os.IsNotExist(err) {
			return nil
		}
		dst := filepath.Join(rollbackDir, name)
		if err := moveFile(original, dst); err != nil {
			return err
		}
		st.Moved = append(st.Moved, movedFile{Original: original, Rollback: dst})
		return writeState(statePath, st)
	}
	var swapErr error
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if swapErr = move(dbPath+suffix, FileDatabase+suffix); swapErr != nil {
			break
		}
	}
	if _, ok := p.Files[FileConfig]; ok && swapErr == nil {
		swapErr = move(configPath, FileConfig)
	}
	for name := range p.Files {
		if swapErr != nil {
			break
		}
		if swapErr = moveFile(filepath.Join(staged, name), targets[name]); swapErr == nil {
			markRestored(st, targets[name])
			swapErr = writeState(statePath, st)
		}
	}
	if swapErr != nil {
		b.rollback(fmt.Errorf("swapping in the restored files failed: %w", swapErr))
		return b, nil
	}
	log.Printf("NEZHA>> restore: swapped in backup %s; previous data kept in %s until the next successful start", p.Name, rollbackDir)
	return b, nil
}

func markRestored(st *restoreState, original string) {
	for i := range st.Moved {
		if st.Moved[i].Original == original {
			st.Moved[i].Restored = true
			return
		}
	}
	// The target did not exist before (e.g. no config.yaml): remember it so a
	// rollback removes the restored file.
	st.Moved = append(st.Moved, movedFile{Original: original, Restored: true})
}

func writeState(path string, st *restoreState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// Commit records a restore whose start succeeded.
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

// Rollback puts the previous files back after a failed start.
func (b *Boot) Rollback(cause error) {
	if !b.Applied() {
		return
	}
	b.rollback(cause)
}

func (b *Boot) rollback(cause error) {
	st := b.state
	var errs []error
	// Journal files next to the database now belong to the restored
	// database (it may have been opened); they must never be replayed onto
	// the previous one. The previous database's own journal files, if any,
	// are moved back from the rollback directory below.
	if st.DBPath != "" {
		for _, suffix := range []string{"-journal", "-wal", "-shm"} {
			if err := os.Remove(st.DBPath + suffix); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
	}
	for i := len(st.Moved) - 1; i >= 0; i-- {
		mv := st.Moved[i]
		if mv.Restored {
			if err := os.Remove(mv.Original); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
		if mv.Rollback == "" {
			continue
		}
		if _, err := os.Lstat(mv.Rollback); err != nil {
			continue
		}
		if err := moveFile(mv.Rollback, mv.Original); err != nil {
			errs = append(errs, err)
		}
	}
	result := RestoreResult{
		Status: "failed", Name: st.Name, SHA256: st.SHA256, AppliedAt: st.AppliedAt,
		FinishedAt: time.Now().UTC(), RolledBack: len(errs) == 0, Error: cause.Error(),
	}
	if len(errs) > 0 {
		result.Error = fmt.Sprintf("%v; rolling back failed: %v (previous data is in %s)", cause, errors.Join(errs...), filepath.Join(b.dataDir, rollbackDirName))
	}
	writeRestoreResult(b.dataDir, result)
	if len(errs) == 0 {
		_ = os.Remove(filepath.Join(b.dataDir, stateFileName))
	}
	log.Printf("NEZHA>> restore: %s", result.Error)
	b.state = nil
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
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600) // #nosec G304 -- dashboard data files
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
	if err != nil {
		return err
	}
	in.Close()
	return os.Remove(src)
}
