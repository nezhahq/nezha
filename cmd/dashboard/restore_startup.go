package main

import (
	"crypto/tls"
	"fmt"
	"net"

	"github.com/nezhahq/nezha/service/singleton"
)

// restoreCommitter is implemented by *backup.Boot (nil-safe).
type restoreCommitter interface {
	Commit()
	Rollback(cause error)
}

// finishRestoreStartup commits a restore applied during this start only
// once the dashboard is ready to serve. If startup failed (configuration,
// database, listeners, TLS of the restored data), the previous data and
// configuration are put back before the process exits, so the supervisor
// starts the dashboard with them again.
func finishRestoreStartup(boot restoreCommitter, startupErr error) error {
	if startupErr == nil {
		boot.Commit()
		return nil
	}
	if singleton.DB != nil {
		if sqlDB, err := singleton.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
	boot.Rollback(startupErr)
	return startupErr
}

// checkHTTPSReady fails when the HTTPS listener configured in the
// (possibly restored) config.yaml could not start: the certificate pair
// does not load or the address cannot be bound. The HTTPS server itself is
// started later in its own goroutine, after a restore has been committed.
func checkHTTPSReady(address string, port uint16, certPath, keyPath string) error {
	if port == 0 {
		return nil
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		return fmt.Errorf("https: load certificate: %w", err)
	}
	l, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("https: %w", err)
	}
	return l.Close()
}
