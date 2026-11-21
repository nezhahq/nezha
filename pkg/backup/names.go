package backup

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"time"
)

// Trigger records why a backup was taken. Retention only ever deletes
// TriggerSchedule backups automatically (and keeps a few TriggerPreRestore).
type Trigger string

const (
	TriggerManual     Trigger = "manual"
	TriggerSchedule   Trigger = "schedule"
	TriggerPreRestore Trigger = "pre-restore"
)

// FileExt is the extension of an encrypted backup object.
const FileExt = ".nzbak"

// ConfirmWordLength is how many trailing characters of a backup name an
// administrator types to confirm a restore.
const ConfirmWordLength = 6

const nameTimeLayout = "20060102-150405"

// nameRe is the only shape of backup names (and object keys below the
// prefix) the dashboard reads, writes or deletes. Everything else in the
// bucket is ignored, so a name can never smuggle a path.
var nameRe = regexp.MustCompile(`^nezha-backup-([0-9]{8}-[0-9]{6})-(manual|schedule|pre-restore)-([0-9a-f]{6})$`)

// NewName returns a fresh backup name for t (always rendered in UTC). The
// random suffix keeps two backups taken in the same second apart and is what
// an administrator types to confirm a restore.
func NewName(t time.Time, trigger Trigger) (string, error) {
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return "nezha-backup-" + t.UTC().Format(nameTimeLayout) + "-" + string(trigger) + "-" + hex.EncodeToString(suffix[:]), nil
}

// ParseName validates name and returns its creation time and trigger.
func ParseName(name string) (time.Time, Trigger, bool) {
	m := nameRe.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, "", false
	}
	t, err := time.ParseInLocation(nameTimeLayout, m[1], time.UTC)
	if err != nil {
		return time.Time{}, "", false
	}
	return t, Trigger(m[2]), true
}

// ValidName reports whether name is a well-formed backup name.
func ValidName(name string) bool {
	_, _, ok := ParseName(name)
	return ok
}

// ConfirmWord is the text an administrator must type to apply a restore of name.
func ConfirmWord(name string) string {
	if len(name) < ConfirmWordLength {
		return name
	}
	return name[len(name)-ConfirmWordLength:]
}

func validTrigger(t Trigger) bool {
	switch t {
	case TriggerManual, TriggerSchedule, TriggerPreRestore:
		return true
	}
	return false
}
