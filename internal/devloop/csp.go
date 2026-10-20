package devloop

import (
	"slices"
	"strings"
)

// allowReload returns policy extended just enough for the reload script to
// run and connect, and only that:
//
//   - the script element needs a nonce unless script elements already allow
//     'self' (a 'strict-dynamic' policy ignores 'self', so it gets the nonce
//     too); the nonce admits the one tag the proxy injects, not scripts in
//     general;
//   - the event stream is a same-origin fetch, so connect-src gains 'self'
//     when it does not allow it.
//
// A policy that relies on 'unsafe-inline' gets 'self' instead of a nonce,
// because a nonce would disable the page's inline scripts. A policy that
// restricts neither is returned unchanged. The proxy applies this in
// development only; the application's own policy is never edited. Policies
// delivered in a <meta http-equiv> tag are not seen by the proxy and are not
// edited: if one blocks the reload script, the browser reports it in the
// console and the page just does not reload itself.
func allowReload(policy, nonce string) string {
	directives := parsePolicy(policy)

	scripts := firstDirective(directives, "script-src-elem", "script-src", "default-src")
	if scripts != nil && !allowsSelfScripts(scripts.sources) {
		token := "'nonce-" + nonce + "'"
		if keepsUnsafeInline(scripts.sources) {
			// A nonce switches 'unsafe-inline' off, which would break the
			// page's own inline scripts in development. The injected tag
			// is same-origin, so admit it by origin instead.
			token = "'self'"
		}
		if scripts.name == "default-src" {
			// Do not widen every fetch type: add an element-specific
			// directive that keeps the default's sources.
			directives = append(directives, directive{name: "script-src-elem", sources: withToken(scripts.sources, token)})
		} else {
			scripts.sources = withToken(scripts.sources, token)
		}
	}

	connect := firstDirective(directives, "connect-src", "default-src")
	if connect != nil && !allowsSelfOrAny(connect.sources) {
		if connect.name == "default-src" {
			directives = append(directives, directive{name: "connect-src", sources: withToken(connect.sources, "'self'")})
		} else {
			connect.sources = withToken(connect.sources, "'self'")
		}
	}
	return formatPolicy(directives)
}

type directive struct {
	name    string
	sources []string
}

func parsePolicy(policy string) []directive {
	var directives []directive
	for part := range strings.SplitSeq(policy, ";") {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		directives = append(directives, directive{name: strings.ToLower(fields[0]), sources: fields[1:]})
	}
	return directives
}

func formatPolicy(directives []directive) string {
	parts := make([]string, 0, len(directives))
	for _, d := range directives {
		parts = append(parts, strings.TrimSpace(d.name+" "+strings.Join(d.sources, " ")))
	}
	return strings.Join(parts, "; ")
}

// firstDirective returns the first of names that the policy sets, following
// the CSP fallback order the caller passes. Duplicates after the first are
// ignored by browsers, so only the first is considered.
func firstDirective(directives []directive, names ...string) *directive {
	for _, name := range names {
		for i := range directives {
			if directives[i].name == name {
				return &directives[i]
			}
		}
	}
	return nil
}

// keepsUnsafeInline reports whether the sources rely on 'unsafe-inline'
// working: it is present and nothing already disables it (a nonce, a hash or
// 'strict-dynamic' each make browsers ignore it).
func keepsUnsafeInline(sources []string) bool {
	unsafeInline := false
	for _, s := range sources {
		lower := strings.ToLower(s)
		switch {
		case lower == "'unsafe-inline'":
			unsafeInline = true
		case lower == "'strict-dynamic'", strings.HasPrefix(lower, "'nonce-"), strings.HasPrefix(lower, "'sha256-"), strings.HasPrefix(lower, "'sha384-"), strings.HasPrefix(lower, "'sha512-"):
			return false
		}
	}
	return unsafeInline
}

func allowsSelfScripts(sources []string) bool {
	has := func(token string) bool {
		return slices.ContainsFunc(sources, func(s string) bool { return strings.EqualFold(s, token) })
	}
	if has("'strict-dynamic'") {
		return false
	}
	return has("'self'") || has("*")
}

func allowsSelfOrAny(sources []string) bool {
	return slices.ContainsFunc(sources, func(s string) bool { return strings.EqualFold(s, "'self'") || s == "*" })
}

// withToken adds token and drops 'none', which cannot be combined with other
// sources.
func withToken(sources []string, token string) []string {
	result := make([]string, 0, len(sources)+1)
	for _, s := range sources {
		if !strings.EqualFold(s, "'none'") {
			result = append(result, s)
		}
	}
	return append(result, token)
}
