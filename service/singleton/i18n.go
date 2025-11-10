package singleton

import (
	"log"
	"strings"

	"github.com/nezhahq/nezha/pkg/i18n"
)

const domain = "nezha"

var Localizer *i18n.Localizer

func initI18n() {
	if err := loadTranslation(); err != nil {
		log.Printf("NEZHA>> init i18n failed: %v", err)
	}
}

func loadTranslation() error {
	lang := Conf.Language
	if lang == "" {
		lang = "zh_CN"
	}

	lang = strings.Replace(lang, "-", "_", 1)
	Localizer = i18n.NewLocalizer(lang, domain, "translations", i18n.Translations)
	return nil
}

func OnUpdateLang(lang string) error {
	lang = strings.Replace(lang, "-", "_", 1)
	if Localizer.Exists(lang) {
		Localizer.SetLanguage(lang)
		return nil
	}

	Localizer.AppendIntl(lang)
	Localizer.SetLanguage(lang)
	return nil
}
