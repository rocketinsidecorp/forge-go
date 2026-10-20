package i18n

import "slices"

// BuiltinPluralLocales lists the locales that have a built-in plural rule.
func BuiltinPluralLocales() []string {
	locales := make([]string, 0, len(builtinRules))
	for locale := range builtinRules {
		locales = append(locales, locale)
	}
	slices.Sort(locales)
	return locales
}
