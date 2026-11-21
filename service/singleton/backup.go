package singleton

import (
	"context"
	"log"
	"path/filepath"

	"github.com/nezhahq/nezha/pkg/backup"
)

// BackupShared runs encrypted backups to S3 compatible storage and staged
// restores. It is nil when the database is not a file (in-memory DSN) or
// the backup settings could not be loaded.
var BackupShared *backup.Manager

// RequestRestart asks the dashboard to shut down gracefully; its supervisor
// (Docker restart policy, systemd Restart=) starts it again. main sets it.
var RequestRestart func()

// InitBackup starts the backup manager. Backups are optional: a failure is
// logged and leaves the feature unavailable instead of stopping the dashboard.
func InitBackup(configPath, dbDSN string) error {
	dbPath := backup.SQLiteFilePath(dbDSN)
	if dbPath == "" {
		log.Println("NEZHA>> backup: disabled, the database is not a file")
		return nil
	}
	m, err := backup.NewManager(backup.Options{
		DataDir:    filepath.Dir(dbPath),
		ConfigPath: configPath,
		Version:    Version,
		Snapshot:   snapshotDatabase,
		CronParser: newCronSpecParser(),
		Location:   Loc,
		Restart: func() {
			if RequestRestart != nil {
				RequestRestart()
			}
		},
	})
	if err != nil {
		log.Printf("NEZHA>> backup: disabled: %v", err)
		return nil
	}
	BackupShared = m
	return nil
}

// snapshotDatabase writes a consistent copy of the live database to dst.
// VACUUM INTO runs in one read transaction, so the copy is a single point in
// time and the dashboard keeps running; writers wait for it like for any
// other reader.
func snapshotDatabase(ctx context.Context, dst string) error {
	return DB.WithContext(ctx).Exec("VACUUM INTO ?", dst).Error
}
