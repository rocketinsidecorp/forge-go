package i18n

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Limits of Accept-Language parsing: longer headers are ignored, and only the
// first entries are considered.
const (
	maxAcceptLanguageBytes   = 4096
	maxAcceptLanguageEntries = 32
)

// DefaultCookie is the cookie that remembers a visitor's locale.
const DefaultCookie = "locale"

// NegotiationOptions configures how Middleware picks a request's locale.
type NegotiationOptions struct {
	// PathPrefix lets the first path segment choose the locale, as in
	// /pt-BR/posts. The segment must name an available locale exactly
	// (ignoring case) and is removed before routing, so /pt-BR/posts is
	// served by the GET /posts route.
	PathPrefix bool
	// Param names a query parameter that chooses the locale, such as
	// "locale" for ?locale=pt-BR. Empty disables it.
	Param string
	// Cookie is the cookie that remembers a visitor's choice (see
	// SetCookie). Empty means DefaultCookie.
	Cookie string
}

// Match returns the available locale that best serves a requested one: the
// locale itself, else its closest parent (pt-BR -> pt), else another
// variant of its language (pt -> pt-BR, choosing the default locale's
// variant first, then the first in sorted order), else the default locale.
// Invalid names get the default locale.
func (t *Translator) Match(requested string) string {
	locale, ok := t.match(requested)
	if !ok {
		return t.defaultLocale
	}
	return locale
}

func (t *Translator) match(requested string) (string, bool) {
	canonical, err := CanonicalLocale(requested)
	if err != nil {
		return "", false
	}
	for _, candidate := range parents(canonical) {
		if _, ok := t.catalog[candidate]; ok {
			return candidate, true
		}
	}
	lang := language(canonical)
	if language(t.defaultLocale) == lang {
		return t.defaultLocale, true
	}
	for _, available := range t.locales {
		if language(available) == lang {
			return available, true
		}
	}
	return "", false
}

// exact returns the available locale named by segment, ignoring case.
func (t *Translator) exact(segment string) (string, bool) {
	canonical, err := CanonicalLocale(segment)
	if err != nil {
		return "", false
	}
	_, ok := t.catalog[canonical]
	return canonical, ok
}

// languageRange is one Accept-Language entry.
type languageRange struct {
	tag     string
	quality int // thousandths
}

// ParseAcceptLanguage returns the language ranges of an Accept-Language
// header with a non-zero quality, highest quality first (ties keep header
// order). Malformed entries are skipped; a header longer than 4096 bytes is
// ignored, and only its first 32 entries are read. "*" is kept as a range.
func ParseAcceptLanguage(header string) []string {
	ranges := parseAcceptLanguage(header)
	if len(ranges) == 0 {
		return nil
	}
	tags := make([]string, len(ranges))
	for i, entry := range ranges {
		tags[i] = entry.tag
	}
	return tags
}

func parseAcceptLanguage(header string) []languageRange {
	if header == "" || len(header) > maxAcceptLanguageBytes {
		return nil
	}
	var ranges []languageRange
	for entries := 0; header != "" && entries < maxAcceptLanguageEntries; entries++ {
		var entry string
		entry, header, _ = strings.Cut(header, ",")
		parts := strings.Split(entry, ";")
		tag := strings.TrimSpace(parts[0])
		if !languageTag(tag) {
			continue
		}
		quality, ok := 1000, true
		for _, parameter := range parts[1:] {
			name, value, found := strings.Cut(strings.TrimSpace(parameter), "=")
			if !found || !strings.EqualFold(strings.TrimSpace(name), "q") {
				continue
			}
			quality, ok = parseQuality(strings.TrimSpace(value))
		}
		if !ok || quality == 0 {
			continue
		}
		ranges = append(ranges, languageRange{tag: tag, quality: quality})
	}
	slices.SortStableFunc(ranges, func(a, b languageRange) int { return b.quality - a.quality })
	return ranges
}

// languageTag accepts "*" and RFC 4647 language ranges: 1-8 letters, then
// "-" and 1-8 letters or digits per subtag.
func languageTag(tag string) bool {
	if tag == "*" {
		return true
	}
	if tag == "" || len(tag) > 64 {
		return false
	}
	for i, subtag := range strings.Split(tag, "-") {
		if subtag == "" || len(subtag) > 8 {
			return false
		}
		for j := range len(subtag) {
			c := subtag[j] | 0x20
			letter := c >= 'a' && c <= 'z'
			digit := subtag[j] >= '0' && subtag[j] <= '9'
			if !letter && (i == 0 || !digit) {
				return false
			}
		}
	}
	return true
}

// parseQuality parses an RFC 9110 qvalue ("0", "0.5", "1.000") into
// thousandths.
func parseQuality(value string) (int, bool) {
	whole, fraction, _ := strings.Cut(value, ".")
	if (whole != "0" && whole != "1") || len(fraction) > 3 || !isDigits(fraction+"0") {
		return 0, false
	}
	thousandths, _ := strconv.Atoi((fraction + "000")[:3])
	if whole == "1" {
		if thousandths != 0 {
			return 0, false
		}
		return 1000, true
	}
	return thousandths, true
}

// matchAcceptLanguage picks the available locale for an Accept-Language
// header, by RFC 4647 lookup with truncation, then by language. "*" picks
// the default locale.
func (t *Translator) matchAcceptLanguage(header string) (string, bool) {
	for _, entry := range parseAcceptLanguage(header) {
		if entry.tag == "*" {
			return t.defaultLocale, true
		}
		// Truncate unsupported subtags (zh-Hant-TW-x-private) until the
		// remainder is a locale Forge can name.
		tag := entry.tag
		for {
			if locale, ok := t.match(tag); ok {
				return locale, true
			}
			cut := strings.LastIndexByte(tag, '-')
			if cut < 0 {
				break
			}
			tag = tag[:cut]
		}
	}
	return "", false
}

// Negotiate returns the locale of a request, the first of: the path prefix
// (with PathPrefix), the query parameter (with Param), the cookie, the
// Accept-Language header and the default locale. Values that name no
// available locale are ignored.
func (t *Translator) Negotiate(r *http.Request, opts NegotiationOptions) string {
	locale, _ := t.negotiate(r, opts)
	return locale
}

// negotiate also returns the path prefix to remove, if any.
func (t *Translator) negotiate(r *http.Request, opts NegotiationOptions) (string, string) {
	if opts.PathPrefix {
		segment, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if locale, ok := t.exact(segment); ok {
			return locale, "/" + segment
		}
	}
	if opts.Param != "" {
		if value := r.URL.Query().Get(opts.Param); value != "" {
			if locale, ok := t.match(value); ok {
				return locale, ""
			}
		}
	}
	if cookie, err := r.Cookie(cookieName(opts)); err == nil {
		if locale, ok := t.match(cookie.Value); ok {
			return locale, ""
		}
	}
	if locale, ok := t.matchAcceptLanguage(r.Header.Get("Accept-Language")); ok {
		return locale, ""
	}
	return t.defaultLocale, ""
}

func cookieName(opts NegotiationOptions) string {
	if opts.Cookie == "" {
		return DefaultCookie
	}
	return opts.Cookie
}

// Middleware negotiates each request's locale (see Negotiate), stores it with
// the translator in the request context for T, the views' t helper and the
// formatting functions, and sends Content-Language and Vary. Install it
// before routing (forge.App.Use) so a path prefix is removed in time.
func (t *Translator) Middleware(opts NegotiationOptions) (func(http.Handler) http.Handler, error) {
	if opts.Param != "" && !argumentName(opts.Param) {
		return nil, fmt.Errorf("i18n: invalid locale parameter name %q", truncate(opts.Param))
	}
	if opts.Cookie != "" && !validCookieName(opts.Cookie) {
		return nil, fmt.Errorf("i18n: invalid locale cookie name %q", truncate(opts.Cookie))
	}
	return func(next http.Handler) http.Handler {
		if next == nil {
			panic("i18n: Middleware needs a next handler")
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			locale, prefix := t.negotiate(r, opts)
			header := w.Header()
			header.Set("Content-Language", locale)
			header.Add("Vary", "Accept-Language")
			header.Add("Vary", "Cookie")
			r = r.WithContext(NewContext(r.Context(), t, locale))
			if prefix != "" {
				r = stripPrefix(r, prefix)
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// stripPrefix removes a locale segment from the request path.
func stripPrefix(r *http.Request, prefix string) *http.Request {
	stripped := *r.URL
	stripped.Path = strings.TrimPrefix(stripped.Path, prefix)
	if stripped.Path == "" {
		stripped.Path = "/"
	}
	if stripped.RawPath != "" {
		stripped.RawPath = strings.TrimPrefix(stripped.RawPath, prefix)
		if stripped.RawPath == "" {
			stripped.RawPath = "/"
		}
	}
	r.URL = &stripped
	return r
}

func validCookieName(name string) bool {
	if len(name) > 64 {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if c != '_' && c != '-' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return name != ""
}

// SetCookie remembers a visitor's locale choice for a year, for example in
// the handler of a language switcher. The cookie is Secure, HttpOnly and
// SameSite=Lax; name empty means DefaultCookie. The locale must be available
// (see Match).
func (t *Translator) SetCookie(w http.ResponseWriter, name, locale string) error {
	if name == "" {
		name = DefaultCookie
	}
	if !validCookieName(name) {
		return fmt.Errorf("i18n: invalid locale cookie name %q", truncate(name))
	}
	available, ok := t.exact(locale)
	if !ok {
		return errors.New("i18n: SetCookie needs an available locale")
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    available,
		Path:     "/",
		MaxAge:   365 * 24 * 60 * 60,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}
