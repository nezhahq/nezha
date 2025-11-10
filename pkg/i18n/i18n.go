package i18n

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sync"

	"github.com/leonelquinteros/gotext"
)

//go:embed translations
var Translations embed.FS

type Localizer struct {
	intlMap map[string]gotext.Translator
	lang    string
	domain  string
	path    string
	fs      fs.FS

	mu sync.RWMutex
}

func NewLocalizer(lang, domain, path string, fs fs.FS) *Localizer {
	loc := &Localizer{
		intlMap: make(map[string]gotext.Translator),
		lang:    lang,
		domain:  domain,
		path:    path,
		fs:      fs,
	}

	file := loc.findExt(lang, "mo")
	if file == "" {
		return loc
	}

	mo := gotext.NewMoFS(loc.fs)
	mo.ParseFile(file)
	loc.intlMap[lang] = mo

	return loc
}

func (l *Localizer) SetLanguage(lang string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.lang = lang
}

func (l *Localizer) Exists(lang string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if _, ok := l.intlMap[lang]; ok {
		return ok
	}
	return false
}

func (l *Localizer) AppendIntl(lang string) {
	file := l.findExt(lang, "mo")
	if file == "" {
		return
	}

	mo := gotext.NewMoFS(l.fs)
	mo.ParseFile(file)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.intlMap[lang] = mo
}

// Modified from k8s.io/kubectl/pkg/util/i18n

func (l *Localizer) T(orig string) string {
	l.mu.RLock()
	intl, ok := l.intlMap[l.lang]
	l.mu.RUnlock()
	if !ok {
		return orig
	}

	return intl.Get(orig)
}

// N translates a string, possibly substituting arguments into it along
// the way. If len(args) is > 0, args1 is assumed to be the plural value
// and plural translation is used.
func (l *Localizer) N(orig string, args ...int) string {
	l.mu.RLock()
	intl, ok := l.intlMap[l.lang]
	l.mu.RUnlock()
	if !ok {
		return orig
	}

	if len(args) == 0 {
		return intl.Get(orig)
	}
	return fmt.Sprintf(intl.GetN(orig, orig+".plural", args[0]),
		args[0])
}

// ErrorT produces an error with a translated error string.
// Substitution is performed via the `T` function above, following
// the same rules.
func (l *Localizer) ErrorT(defaultValue string, args ...any) error {
	return fmt.Errorf(l.T(defaultValue), args...)
}

func (l *Localizer) Tf(defaultValue string, args ...any) string {
	return fmt.Sprintf(l.T(defaultValue), args...)
}

// https://github.com/leonelquinteros/gotext/blob/v1.7.1/locale.go
func (l *Localizer) findExt(lang, ext string) string {
	filename := path.Join(l.path, lang, "LC_MESSAGES", l.domain+"."+ext)
	if l.fileExists(filename) {
		return filename
	}

	if len(lang) > 2 {
		filename = path.Join(l.path, lang[:2], "LC_MESSAGES", l.domain+"."+ext)
		if l.fileExists(filename) {
			return filename
		}
	}

	filename = path.Join(l.path, lang, l.domain+"."+ext)
	if l.fileExists(filename) {
		return filename
	}

	if len(lang) > 2 {
		filename = path.Join(l.path, lang[:2], l.domain+"."+ext)
		if l.fileExists(filename) {
			return filename
		}
	}

	return ""
}

func (l *Localizer) fileExists(filename string) bool {
	_, err := fs.Stat(l.fs, filename)
	return err == nil
}
