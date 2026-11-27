//go:build !windows

package backup

import (
	"errors"
	"os"
	"syscall"
)

// syncDirectory fsyncs a directory. EINVAL / ENOTSUP mean the file system
// does not support syncing directories at all (some network and FUSE file
// systems); there is nothing more that can be done there, so they count as
// success. Every other error is returned.
func syncDirectory(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- dashboard data directory
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}
