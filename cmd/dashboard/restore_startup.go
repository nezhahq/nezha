package main

import (
	"crypto/tls"
	"fmt"
	"net"

	"github.com/nezhahq/nezha/service/singleton"
)

// restoreCommitter is implemented by *backup.Boot (nil-safe).
type restoreCommitter interface {
	Commit() error
	Rollback(cause error)
}

// finishRestoreStartup commits a restore applied during this start only
// once the dashboard is ready to serve. If startup failed (configuration,
// database, listeners, TLS of the restored data), the previous data and
// configuration are put back before the process exits, so the supervisor
// starts the dashboard with them again. If the commit itself cannot be
// recorded, the dashboard must not serve either: the restore stays planned
// and the next start rolls it back.
func finishRestoreStartup(boot restoreCommitter, startupErr error) error {
	if startupErr == nil {
		if err := boot.Commit(); err != nil {
			closeDatabase()
			return err
		}
		return nil
	}
	closeDatabase()
	boot.Rollback(startupErr)
	return startupErr
}

func closeDatabase() {
	if singleton.DB != nil {
		if sqlDB, err := singleton.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
}

// openDashboardHTTPS opens the listener and loads the certificate the HTTPS
// server will use, before a restore is committed. The listener comes from
// the same selection as every dashboard listener (an inherited socket in
// builds that adopt one, otherwise a bind), and the server later serves on
// exactly this listener with exactly this certificate, so nothing that was
// checked can change in between. It returns nil when HTTPS is disabled.
func openDashboardHTTPS(address string, port uint16, certPath, keyPath string) (net.Listener, *tls.Certificate, error) {
	if port == 0 {
		return nil, nil, nil
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("https: load certificate: %w", err)
	}
	l, err := openDashboardListener("tcp", address, dashboardHTTPSListener)
	if err != nil {
		return nil, nil, fmt.Errorf("https: %w", err)
	}
	return l, &cert, nil
}
