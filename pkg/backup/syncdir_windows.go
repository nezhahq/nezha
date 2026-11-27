//go:build windows

package backup

// syncDirectory is a no-op on Windows: directories cannot be opened for
// FlushFileBuffers there, and NTFS journals directory metadata itself. The
// renamed or removed files themselves are flushed before they are renamed.
func syncDirectory(string) error { return nil }
