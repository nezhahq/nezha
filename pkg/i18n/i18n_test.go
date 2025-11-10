package i18n

import (
	"testing"
)

func TestI18n(t *testing.T) {
	const testStr = "database error"

	t.Run("SwitchLocale", func(t *testing.T) {
		loc := NewLocalizer("zh_CN", "nezha", "translations", Translations)
		translated := loc.T(testStr)
		if translated != "数据库错误" {
			t.Fatalf("expected %s, but got %s", "数据库错误", translated)
		}

		loc.AppendIntl("zh_TW")
		loc.SetLanguage("zh_TW")
		translated = loc.T(testStr)
		if translated != "資料庫錯誤" {
			t.Fatalf("expected %s, but got %s", "資料庫錯誤", translated)
		}
	})

	t.Run("Fallback", func(t *testing.T) {
		loc := NewLocalizer("invalid", "nezha", "translations", Translations)
		fallbackStr := loc.T(testStr)
		if fallbackStr != testStr {
			t.Fatalf("expected %s, but got %s", testStr, fallbackStr)
		}
	})
}
