// Package i18n translates an application's user-visible text, like Rails
// I18n: translations live in JSON files (config/locales/LOCALE.json), keys are
// dotted paths ("users.welcome"), values interpolate named arguments
// ("Welcome, %{name}!") and choose CLDR plural forms by count. Middleware
// picks each request's locale; T, the views' t helper and the formatting
// functions read it from the request context.
//
// The file format, key rules, plural rules, locale negotiation and
// missing-translation behavior are language-neutral (ADR 0042), so
// Forge-Rust reads the same files with the same results. See docs/I18N.md.
package i18n

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// DefaultLocale is the default locale when Options.DefaultLocale is empty.
const DefaultLocale = "en"

// maxWarnings bounds how many distinct missing translations are logged, so a
// key built from request input cannot grow memory or flood the log.
const maxWarnings = 1024

// Options configures Load.
type Options struct {
	// DefaultLocale is the locale used when a request asks for none Forge
	// has, and the last fallback for missing translations in production. It
	// must have a translation file. Empty means "en".
	DefaultLocale string
	// ShowMissing makes a missing translation visible as
	// "translation missing: LOCALE.KEY" (development and test). Otherwise
	// (staging and production) the default locale's text is used, then the
	// key's last segment, humanized ("welcome_back" -> "Welcome back"). Both
	// log a warning once per locale and key.
	ShowMissing bool
	// Logger receives missing-translation warnings. Nil discards them.
	Logger *slog.Logger
	// PluralRules adds or replaces plural rules by canonical locale, for
	// languages without a built-in rule (see PluralRuleFor).
	PluralRules map[string]PluralRule
}

// Translator holds an application's translations. It is immutable after
// Load and safe for concurrent use.
type Translator struct {
	defaultLocale string
	locales       []string
	catalog       map[string]tree
	showMissing   bool
	logger        *slog.Logger
	rules         map[string]PluralRule

	mu     sync.Mutex
	warned map[string]struct{}
}

// Load reads the translation files at the root of fsys: LOCALE.json and
// SCOPE.LOCALE.json (such as en.json and posts.en.json), each one object
// whose only key is the locale. The files of one locale are merged; a key
// defined twice is an error. The available locales are the ones with files.
func Load(fsys fs.FS, opts Options) (*Translator, error) {
	defaultLocale := opts.DefaultLocale
	if defaultLocale == "" {
		defaultLocale = DefaultLocale
	}
	defaultLocale, err := CanonicalLocale(defaultLocale)
	if err != nil {
		return nil, fmt.Errorf("default locale: %w", err)
	}
	catalog, err := readCatalog(fsys)
	if err != nil {
		return nil, err
	}
	if _, ok := catalog[defaultLocale]; !ok {
		return nil, fmt.Errorf("i18n: the default locale %s has no translation file (%s.json)", defaultLocale, defaultLocale)
	}
	rules := make(map[string]PluralRule, len(opts.PluralRules))
	for locale, rule := range opts.PluralRules {
		canonical, err := CanonicalLocale(locale)
		if err != nil {
			return nil, fmt.Errorf("plural rule: %w", err)
		}
		if rule == nil {
			return nil, fmt.Errorf("i18n: plural rule for %s is nil", canonical)
		}
		rules[canonical] = rule
	}
	locales := make([]string, 0, len(catalog))
	for locale := range catalog {
		locales = append(locales, locale)
	}
	slices.Sort(locales)
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Translator{
		defaultLocale: defaultLocale,
		locales:       locales,
		catalog:       catalog,
		showMissing:   opts.ShowMissing,
		logger:        logger,
		rules:         rules,
		warned:        map[string]struct{}{},
	}, nil
}

// DefaultLocale returns the default locale.
func (t *Translator) DefaultLocale() string { return t.defaultLocale }

// Locales returns the available locales, sorted.
func (t *Translator) Locales() []string { return slices.Clone(t.locales) }

// Keys returns the translation keys of a locale, sorted; plural forms count
// as one key. An unknown locale has none.
func (t *Translator) Keys(locale string) []string {
	node, ok := t.catalog[locale]
	if !ok {
		return nil
	}
	return leafKeys(node)
}

// PluralCategories returns the plural forms the locale's rule uses for the
// counts 0 to 1000, the forms each plural translation of that locale should
// provide.
func (t *Translator) PluralCategories(locale string) []Category {
	return usedCategories(ruleIn(t.rules, locale))
}

// PluralForms returns the forms a plural translation defines in a locale, or
// false when key is not a plural translation there.
func (t *Translator) PluralForms(locale, key string) ([]Category, bool) {
	node, ok := t.catalog[locale]
	if !ok {
		return nil, false
	}
	value, ok := find(node, key)
	forms, isTree := value.(tree)
	if !ok || !isTree || !isPlural(forms) {
		return nil, false
	}
	var present []Category
	for _, category := range categories {
		if _, ok := forms[string(category)]; ok {
			present = append(present, category)
		}
	}
	return present, true
}

// Has reports whether key has a translation in locale itself (not through a
// fallback).
func (t *Translator) Has(locale, key string) bool {
	node, ok := t.catalog[locale]
	if !ok {
		return false
	}
	value, ok := find(node, key)
	if !ok {
		return false
	}
	if forms, isTree := value.(tree); isTree {
		return isPlural(forms)
	}
	_, isText := value.(string)
	return isText
}

// Translate returns the text of key in locale, which is matched to an
// available locale like an Accept-Language entry (unknown locales use the
// default). args are name/value pairs interpolated into %{name}; a "count"
// argument also selects the plural form. For keys ending in "_html" (or
// whose last segment is "html") the text is trusted markup and the values
// are HTML-escaped, unless they are template.HTML; other text is returned as
// written. Translate never panics: problems are logged and visible in the
// result (see Options.ShowMissing).
func (t *Translator) Translate(locale, key string, args ...any) string {
	text, _ := t.translate(t.Match(locale), key, args)
	return text
}

// HTML is Translate for an HTML context: "_html" keys return their markup
// with escaped values, any other key returns its text escaped.
func (t *Translator) HTML(locale, key string, args ...any) template.HTML {
	return t.html(t.Match(locale), key, args)
}

func (t *Translator) html(locale, key string, args []any) template.HTML {
	text, markup := t.translate(locale, key, args)
	if markup {
		// #nosec G203 -- an _html translation from the application's own files, every interpolated value HTML-escaped.
		return template.HTML(text)
	}
	// #nosec G203 -- escaped here.
	return template.HTML(template.HTMLEscapeString(text))
}

// IsHTMLKey reports whether key names trusted markup: its last segment is
// "html" or ends in "_html", as in Rails.
func IsHTMLKey(key string) bool {
	last := key[strings.LastIndexByte(key, '.')+1:]
	return last == "html" || strings.HasSuffix(last, "_html")
}

// translate returns the text and whether it is markup (an _html key that was
// found). locale must be available.
func (t *Translator) translate(locale, key string, args []any) (string, bool) {
	values, count, hasCount, argErr := parseArgs(args)
	if argErr != nil {
		t.warn("i18n arguments", locale, key, argErr)
	}
	if !validKey(key) {
		t.warn("invalid translation key", locale, key, nil)
		return t.missing(locale, key), false
	}
	text, found, ok := t.lookup(locale, key, count, hasCount)
	if !ok {
		t.warn("translation missing", locale, key, nil)
		if !t.showMissing {
			for _, fallback := range parents(t.defaultLocale) {
				if text, found, ok = t.lookup(fallback, key, count, hasCount); ok {
					break
				}
			}
		}
		if !ok {
			return t.missing(locale, key), false
		}
	}
	markup := IsHTMLKey(key)
	result, unknown := interpolate(text, values, markup)
	if unknown != "" {
		t.warn("translation argument missing", found, key, fmt.Errorf("no value for %%{%s}", unknown))
	}
	return result, markup
}

// lookup finds key in locale and its parents, choosing the plural form when
// a count is given. found is the locale the text came from.
func (t *Translator) lookup(locale, key string, count Operands, hasCount bool) (text, found string, ok bool) {
	for _, candidate := range parents(locale) {
		node, exists := t.catalog[candidate]
		if !exists {
			continue
		}
		value, exists := find(node, key)
		if !exists {
			continue
		}
		switch typed := value.(type) {
		case string:
			return typed, candidate, true
		case tree:
			if !isPlural(typed) {
				continue
			}
			if !hasCount {
				return typed[string(Other)].(string), candidate, true
			}
			return t.pluralForm(candidate, typed, count), candidate, true
		}
	}
	return "", "", false
}

// pluralForm picks a plural form. An explicit "zero" form is used for 0 in
// every language (as in Rails); a form the rule selects but the file lacks
// falls back to "other".
func (t *Translator) pluralForm(locale string, forms tree, count Operands) string {
	if count.N == 0 && count.V == 0 {
		if text, ok := forms[string(Zero)].(string); ok {
			return text
		}
	}
	category := ruleIn(t.rules, locale)(count)
	if text, ok := forms[string(category)].(string); ok {
		return text
	}
	return forms[string(Other)].(string)
}

// missing is the text of a translation that was not found.
func (t *Translator) missing(locale, key string) string {
	if t.showMissing {
		return "translation missing: " + locale + "." + truncate(key)
	}
	return Humanize(key)
}

// warn logs a problem once per locale and key, for the first maxWarnings
// distinct problems.
func (t *Translator) warn(message, locale, key string, err error) {
	id := message + "\x00" + locale + "\x00" + key
	t.mu.Lock()
	_, seen := t.warned[id]
	full := len(t.warned) >= maxWarnings
	if !seen && !full {
		t.warned[id] = struct{}{}
	}
	t.mu.Unlock()
	if seen || full {
		return
	}
	attrs := []any{"locale", locale, "key", truncate(key)}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	t.logger.Warn(message, attrs...)
}

// Humanize turns a key's last segment into text: "users.welcome_back" gives
// "Welcome back"; a trailing "_html" is dropped.
func Humanize(key string) string {
	last := key[strings.LastIndexByte(key, '.')+1:]
	last = strings.TrimSuffix(last, "_html")
	last = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(last, "_", " "), "-", " "))
	if last == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(last)
	return string(unicode.ToUpper(first)) + last[size:]
}

func validKey(key string) bool {
	if key == "" || len(key) > MaxKeyLength {
		return false
	}
	for segment := range strings.SplitSeq(key, ".") {
		if !segmentPattern.MatchString(segment) {
			return false
		}
	}
	return true
}

// parseArgs reads name/value pairs. Malformed pairs are reported and
// skipped.
func parseArgs(args []any) (map[string]any, Operands, bool, error) {
	if len(args) == 0 {
		return nil, Operands{}, false, nil
	}
	var problem error
	values := make(map[string]any, len(args)/2)
	if len(args)%2 != 0 {
		problem = errors.New("arguments must be name/value pairs")
		args = args[:len(args)-1]
	}
	for i := 0; i < len(args); i += 2 {
		name, ok := args[i].(string)
		if !ok || !argumentName(name) {
			problem = fmt.Errorf("argument %d is not a name such as \"count\"", i/2+1)
			continue
		}
		values[name] = args[i+1]
	}
	var count Operands
	hasCount := false
	if value, ok := values["count"]; ok {
		if count, hasCount = NewOperands(value); !hasCount {
			problem = fmt.Errorf("count is %T, not a number", value)
		}
	}
	return values, count, hasCount, problem
}

func argumentName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// interpolate replaces %{name} with values and %% with %. In markup, values
// are HTML-escaped unless they are template.HTML. unknown names the first
// placeholder without a value, which stays as written.
func interpolate(text string, values map[string]any, markup bool) (result, unknown string) {
	if !strings.Contains(text, "%") {
		return text, ""
	}
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '%' || i+1 == len(text) {
			out.WriteByte(c)
			continue
		}
		switch text[i+1] {
		case '%':
			out.WriteByte('%')
			i++
			continue
		case '{':
			end := strings.IndexByte(text[i+2:], '}')
			if end < 0 {
				break
			}
			name := text[i+2 : i+2+end]
			if !argumentName(name) {
				break
			}
			value, ok := values[name]
			if !ok {
				if unknown == "" {
					unknown = name
				}
				out.WriteString(text[i : i+3+end])
			} else {
				out.WriteString(formatValue(value, markup))
			}
			i += 2 + end
			continue
		}
		out.WriteByte(c)
	}
	return out.String(), unknown
}

func formatValue(value any, markup bool) string {
	if markup {
		if trusted, ok := value.(template.HTML); ok {
			return string(trusted)
		}
		return template.HTMLEscapeString(fmt.Sprint(value))
	}
	return fmt.Sprint(value)
}

type contextKey struct{}

type binding struct {
	translator *Translator
	locale     string
}

// NewContext returns ctx carrying the translator and a locale, matched to an
// available one. Middleware does this for requests; jobs and mailers use it
// to translate for a recipient's locale.
func NewContext(ctx context.Context, t *Translator, locale string) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, binding{translator: t, locale: t.Match(locale)})
}

// FromContext returns the translator and locale of ctx.
func FromContext(ctx context.Context) (*Translator, string, bool) {
	if ctx == nil {
		return nil, "", false
	}
	bound, ok := ctx.Value(contextKey{}).(binding)
	return bound.translator, bound.locale, ok
}

// Locale returns the locale of ctx, or "" without one.
func Locale(ctx context.Context) string {
	_, locale, _ := FromContext(ctx)
	return locale
}

// T translates key for the locale of ctx (see Translator.Translate). Without
// a translator in ctx (no Middleware or NewContext) it returns the key
// humanized.
func T(ctx context.Context, key string, args ...any) string {
	t, locale, ok := FromContext(ctx)
	if !ok {
		return Humanize(key)
	}
	text, _ := t.translate(locale, key, args)
	return text
}

// HTML translates key for the locale of ctx as HTML (see Translator.HTML).
func HTML(ctx context.Context, key string, args ...any) template.HTML {
	t, locale, ok := FromContext(ctx)
	if !ok {
		// #nosec G203 -- escaped here.
		return template.HTML(template.HTMLEscapeString(Humanize(key)))
	}
	return t.html(locale, key, args)
}
