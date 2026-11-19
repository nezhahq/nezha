package utils

import "os"

// RestrictFileToOwner removes group and other access from a file that holds
// secrets. The permission passed to os.WriteFile / os.OpenFile (and SQLite's
// own 0644) is only used when the file gets created, so a file that already
// exists keeps its mode unless it is changed explicitly.
func RestrictFileToOwner(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	perm := info.Mode().Perm()
	if perm&0077 == 0 {
		return nil
	}
	return os.Chmod(path, perm&^0077)
}
