package backup

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3" // sqlite3 driver, also used by the dashboard
)

// SQLiteFilePath returns the file behind a go-sqlite3 DSN as used by the
// dashboard's -db flag, or "" for in-memory, temporary and file: URI
// databases, which backups do not support.
func SQLiteFilePath(dsn string) string {
	if strings.HasPrefix(dsn, "file:") {
		return ""
	}
	if pos := strings.IndexRune(dsn, '?'); pos >= 1 {
		dsn = dsn[:pos]
	}
	if dsn == "" || dsn == ":memory:" {
		return ""
	}
	return dsn
}

func sqliteURI(path string, mode string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String() + "?mode=" + mode, nil
}

type dbInspection struct {
	integrityOK bool
	hasAdmin    bool
	tables      map[string]int64
}

// inspectDatabase opens a database file read-only and runs the integrity
// check, counts the rows of every table and checks for an administrator.
func inspectDatabase(path string) (*dbInspection, error) {
	dsn, err := sqliteURI(path, "ro")
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	out := &dbInspection{tables: map[string]int64{}}
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return nil, fmt.Errorf("integrity check: %w", err)
	}
	out.integrityOK = result == "ok"
	if !out.integrityOK {
		return out, nil
	}
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, n := range names {
		var count int64
		if err := db.QueryRow(`SELECT COUNT(*) FROM "` + strings.ReplaceAll(n, `"`, `""`) + `"`).Scan(&count); err != nil {
			return nil, err
		}
		out.tables[n] = count
	}
	if _, ok := out.tables["users"]; ok {
		var admins int64
		// model.RoleAdmin is 0.
		if err := db.QueryRow("SELECT COUNT(*) FROM users WHERE role = 0").Scan(&admins); err != nil {
			return nil, err
		}
		out.hasAdmin = admins > 0
	}
	return out, nil
}

// clearSessions deletes every login session from a staged database, so that
// nobody stays signed in with a session that existed when the backup was taken.
func clearSessions(path string) error {
	dsn, err := sqliteURI(path, "rw")
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec("DELETE FROM jwt_sessions")
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return nil
	}
	return errors.Join(err)
}
