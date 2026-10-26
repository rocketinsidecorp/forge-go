package i18n

import (
	"context"
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
// available locale are ignored. On a request whose prefix the Middleware
// already removed, the removed prefix (PathPrefix) is the path prefix.
func (t *Translator) Negotiate(r *http.Request, opts NegotiationOptions) string {
	if opts.PathPrefix {
		if locale, ok := t.prefixLocale(r); ok {
			return locale
		}
	}
	if opts.Param != "" {
		if value := r.URL.Query().Get(opts.Param); value != "" {
			if locale, ok := t.match(value); ok {
				return locale
			}
		}
	}
	if cookie, err := r.Cookie(cookieName(opts)); err == nil {
		if locale, ok := t.match(cookie.Value); ok {
			return locale
		}
	}
	if locale, ok := t.matchAcceptLanguage(r.Header.Get("Accept-Language")); ok {
		return locale
	}
	return t.defaultLocale
}

// prefixLocale returns the locale of the path prefix: the one a Middleware
// removed, or, for a request no Middleware rewrote, the first path segment.
func (t *Translator) prefixLocale(r *http.Request) (string, bool) {
	if prefixChecked(r.Context()) {
		return t.exact(strings.TrimPrefix(PathPrefix(r.Context()), "/"))
	}
	return t.exact(firstSegment(r))
}

// firstSegment returns the first segment of the decoded request path.
func firstSegment(r *http.Request) string {
	segment, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	return segment
}

func cookieName(opts NegotiationOptions) string {
	if opts.Cookie == "" {
		return DefaultCookie
	}
	return opts.Cookie
}

// Middleware negotiates each request's locale (see Negotiate) and stores it
// with the translator in the request context for T, the views' t helper and
// the formatting functions. Install it before routing (forge.App.Use). It
// sets no response headers: responses that do not depend on the language
// (digested assets, JSON) stay cacheable; translated responses call
// SetHeaders (the views renderer does).
//
// With PathPrefix, the locale segment is removed from the request path once
// per request (PathPrefix gives it back). The handler the middleware returns
// is an httpserver.Rewriter: in a forge.App, or a server built by
// httpserver, the segment is removed before the first application
// middleware runs, wherever this middleware is registered (App.Use,
// forge.WithHTTPMiddleware), so a path guard registered before it sees
// /admin for /pt-BR/admin, the path the router serves. Elsewhere (a plain
// net/http server, or this middleware wrapped inside another one) it is
// removed when this middleware runs, so only what runs after it sees the
// stripped path. Register it directly: wrapped inside another middleware,
// Forge cannot see it as an httpserver.Rewriter, and path guards outside it
// see the prefix.
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
		return &negotiation{translator: t, opts: opts, next: next}
	}, nil
}

// negotiation is the handler of Middleware.
type negotiation struct {
	translator *Translator
	opts       NegotiationOptions
	next       http.Handler
}

// RewriteRequest removes the locale path prefix (with PathPrefix), unless
// a Middleware already looked for one on this request: the prefix is
// removed at most once, so /pt-BR/en/admin is routed as /en/admin. It
// implements httpserver.Rewriter.
func (n *negotiation) RewriteRequest(r *http.Request) *http.Request {
	if !n.opts.PathPrefix || prefixChecked(r.Context()) {
		return r
	}
	r = r.WithContext(context.WithValue(r.Context(), checkedKey{}, true))
	segment := firstSegment(r)
	locale, ok := n.translator.exact(segment)
	if !ok {
		return r
	}
	r = stripPrefix(r, "/"+segment)
	// The locale's canonical name, not the request's spelling of it
	// ("/PT-br" is stored as "/pt-BR").
	return r.WithContext(context.WithValue(r.Context(), prefixKey{}, "/"+locale)) //nolint:contextcheck // a derived request context: it wraps r.Context() with a value
}

func (n *negotiation) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A no-op when the server already applied the rewrite.
	r = n.RewriteRequest(r)
	locale := n.translator.Negotiate(r, n.opts)
	n.next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), n.translator, locale))) //nolint:contextcheck // a derived request context: it wraps r.Context() with a value
}

type (
	prefixKey  struct{}
	checkedKey struct{}
)

// prefixChecked reports whether a Middleware with PathPrefix already looked
// for a locale path prefix on the request of ctx.
func prefixChecked(ctx context.Context) bool {
	checked, _ := ctx.Value(checkedKey{}).(bool)
	return checked
}

// PathPrefix returns the locale path segment the Middleware removed from the
// request ("/pt-BR"), or "" when the request had none (or ctx did not pass
// through the middleware with PathPrefix on). Code that builds a URL from
// the stripped request path, such as the sign-in return path, puts it back
// in front so the visitor returns to the page in the language they used.
// The value is an available locale's canonical name (letters, digits and
// hyphens) behind a slash, never request text; still validate the finished
// URL before redirecting to it.
func PathPrefix(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	prefix, _ := ctx.Value(prefixKey{}).(string)
	return prefix
}

// SetHeaders marks a response as translated for the locale of ctx:
// Content-Language names it, and Vary: Accept-Language, Cookie keeps shared
// caches from serving one visitor's language to another. The views renderer
// calls it for every page; handlers that write translated text themselves
// call it before writing. Without a locale in ctx it does nothing.
func SetHeaders(w http.ResponseWriter, ctx context.Context) {
	locale := Locale(ctx)
	if locale == "" {
		return
	}
	header := w.Header()
	header.Set("Content-Language", locale)
	for _, name := range []string{"Accept-Language", "Cookie"} {
		if !varies(header, name) {
			header.Add("Vary", name)
		}
	}
}

func varies(header http.Header, name string) bool {
	for _, value := range header.Values("Vary") {
		for field := range strings.SplitSeq(value, ",") {
			if field = strings.TrimSpace(field); field == "*" || strings.EqualFold(field, name) {
				return true
			}
		}
	}
	return false
}

// stripPrefix removes a locale segment from the request path.
func stripPrefix(r *http.Request, prefix string) *http.Request {
	stripped := *r.URL
	stripped.Path = strings.TrimPrefix(stripped.Path, prefix)
	if stripped.Path == "" {
		stripped.Path = "/"
	}
	if stripped.RawPath != "" {
		// Keep the escaped form (a%2Fb) only while it still starts with the
		// whole segment; for /pt-BR%2F..%2Fadmin the decoded segment ends
		// earlier than the escaped one, so the escaped path is dropped and
		// derived from Path again, never left without its leading slash.
		rest, ok := strings.CutPrefix(stripped.RawPath, prefix)
		switch {
		case !ok || (rest != "" && rest[0] != '/'):
			stripped.RawPath = ""
		case rest == "":
			stripped.RawPath = "/"
		default:
			stripped.RawPath = rest
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
