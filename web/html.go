package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// ErrUnsafeRedirect is returned by Redirect for a target that is not a local
// path.
var ErrUnsafeRedirect = fault.New("unsafe_redirect", "redirect target must be a local path", fault.CategoryInvalid, http.StatusBadRequest)

const maxRedirectTarget = 2048

// Redirect answers 303 See Other to a local path such as "/posts/0190..." or
// "/posts?page=2". It writes nothing and returns ErrUnsafeRedirect when
// target could leave the site: absolute and scheme-relative URLs ("//host"),
// backslashes (which browsers treat as slashes), and control, space or
// non-ASCII bytes are all rejected. Use it with values derived from user input
// (for example a return_to parameter) as well as constants.
func Redirect(w http.ResponseWriter, target string) error {
	if !IsLocalPath(target) {
		return ErrUnsafeRedirect
	}
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusSeeOther)
	return nil
}

// IsLocalPath reports whether target is a same-origin absolute path that is
// safe to use as a redirect location. See Redirect.
func IsLocalPath(target string) bool {
	if target == "" || len(target) > maxRedirectTarget || target[0] != '/' {
		return false
	}
	if len(target) > 1 && (target[1] == '/' || target[1] == '\\') {
		return false
	}
	for i := range len(target) {
		if c := target[i]; c <= ' ' || c >= 0x7f || c == '\\' {
			return false
		}
	}
	parsed, err := url.Parse(target)
	return err == nil && parsed.Scheme == "" && parsed.Host == "" && parsed.User == nil && parsed.Opaque == ""
}

const maxAcceptRanges = 32

type acceptRange struct {
	typ, subtype string
	quality      float64
}

// Negotiate returns the offer (a media type such as "text/html") the Accept
// header prefers, using quality values and the most specific matching range
// for each offer. Ties go to the earlier offer, and a request without Accept
// gets the first offer. It returns "" when every offer is refused (q=0).
func Negotiate(r *http.Request, offers ...string) string {
	if len(offers) == 0 {
		return ""
	}
	ranges := parseAccept(r.Header.Values("Accept"))
	if len(ranges) == 0 {
		return offers[0]
	}
	best, bestQuality := "", 0.0
	for _, offer := range offers {
		typ, subtype, ok := strings.Cut(strings.ToLower(offer), "/")
		if !ok {
			continue
		}
		quality, specificity := 0.0, -1
		for _, accepted := range ranges {
			current := -1
			switch {
			case accepted.typ == typ && accepted.subtype == subtype:
				current = 2
			case accepted.typ == typ && accepted.subtype == "*":
				current = 1
			case accepted.typ == "*" && accepted.subtype == "*":
				current = 0
			}
			if current > specificity {
				specificity, quality = current, accepted.quality
			}
		}
		if quality > bestQuality {
			best, bestQuality = offer, quality
		}
	}
	return best
}

// AcceptsExplicitly reports whether the Accept header names mediaType itself
// with a quality above zero. Wildcards ("*/*", "text/*") do not count, so a
// client that accepts anything is not mistaken for one that asked for a
// special type, such as Turbo's text/vnd.turbo-stream.html.
func AcceptsExplicitly(r *http.Request, mediaType string) bool {
	typ, subtype, ok := strings.Cut(strings.ToLower(mediaType), "/")
	if !ok {
		return false
	}
	for _, accepted := range parseAccept(r.Header.Values("Accept")) {
		if accepted.typ == typ && accepted.subtype == subtype && accepted.quality > 0 {
			return true
		}
	}
	return false
}

// PrefersHTML reports whether the client prefers text/html over
// application/json. Browser navigations and form submissions do; API clients,
// fetch() defaults ("*/*") and requests without Accept do not.
func PrefersHTML(r *http.Request) bool {
	return Negotiate(r, "application/json", "text/html") == "text/html"
}

func parseAccept(headers []string) []acceptRange {
	var ranges []acceptRange
	for _, header := range headers {
		for item := range strings.SplitSeq(header, ",") {
			if len(ranges) == maxAcceptRanges {
				return ranges
			}
			mediaRange, params, _ := strings.Cut(strings.TrimSpace(item), ";")
			typ, subtype, ok := strings.Cut(strings.ToLower(strings.TrimSpace(mediaRange)), "/")
			if !ok || typ == "" || subtype == "" || (typ == "*" && subtype != "*") {
				continue
			}
			quality := 1.0
			for param := range strings.SplitSeq(params, ";") {
				key, value, _ := strings.Cut(strings.TrimSpace(param), "=")
				if strings.EqualFold(strings.TrimSpace(key), "q") {
					parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
					if err != nil || parsed < 0 || parsed > 1 {
						parsed = 0
					}
					quality = parsed
				}
			}
			ranges = append(ranges, acceptRange{typ: typ, subtype: subtype, quality: quality})
		}
	}
	return ranges
}

// MethodOverride lets HTML forms, which can only send GET and POST, reach
// PUT, PATCH and DELETE routes: a POST with an
// application/x-www-form-urlencoded body whose _method field is put, patch or
// delete (any case) is routed as that method. Nothing else is rewritten: the
// query string is ignored, other methods and content types pass through, and
// any other _method value is 400 invalid_method_override. The body is read
// through ParseForm, so its size and field limits apply. Install it before
// routing and outside (before) CSRF protection, so CSRF checks and exemptions
// see the routed method. An override never produces a safe method, and the
// csrf package never exempts a form POST that carries _method.
func MethodOverride(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !IsForm(r) {
			next.ServeHTTP(w, r)
			return
		}
		values, err := ParseForm(r)
		if err != nil {
			Error(w, r, err)
			return
		}
		requested, present := values[MethodOverrideField]
		if !present {
			next.ServeHTTP(w, r)
			return
		}
		method := ""
		if len(requested) == 1 {
			method = strings.ToUpper(requested[0])
		}
		switch method {
		case http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			Error(w, r, fault.New("invalid_method_override", "_method must be PUT, PATCH or DELETE", fault.CategoryInvalid, http.StatusBadRequest))
			return
		}
		overridden := r.WithContext(r.Context())
		overridden.Method = method
		next.ServeHTTP(w, overridden)
	})
}
