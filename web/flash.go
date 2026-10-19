package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// FlashCookieName is the cookie that carries flash messages to the next
// request. The __Host- prefix makes browsers refuse it unless it is Secure,
// has Path=/ and no Domain, so a sibling subdomain cannot plant one.
const FlashCookieName = "__Host-forge_flash"

// Flash limits: the encoded cookie value, the messages per response and the
// bytes of one message.
const (
	MaxFlashCookieBytes = 2048
	MaxFlashMessages    = 4
	MaxFlashTextBytes   = 512
)

// FlashKind names a flash message. Kinds are lowercase letters, digits and
// underscores, starting with a letter, up to 32 bytes.
type FlashKind string

const (
	FlashNotice FlashKind = "notice"
	FlashAlert  FlashKind = "alert"
)

// FlashMessage is one message. Templates render Text with contextual
// escaping, like any other value.
type FlashMessage struct {
	Kind FlashKind
	Text string
}

// ErrFlashUnavailable is returned by SetFlash when the Flash middleware is
// not installed.
var ErrFlashUnavailable = errors.New("web: flash middleware is not installed")

var flashKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

type flashKey struct{}

type flashState struct {
	mu       sync.Mutex
	incoming []FlashMessage
	outgoing []FlashMessage
}

// wireFlash is the cookie encoding: a JSON array, base64url without padding.
type wireFlash struct {
	Kind string `json:"k"`
	Text string `json:"t"`
}

// Flash is middleware that carries one-request messages, such as "Post was
// created." after a redirect, in the FlashCookieName cookie. It loads the
// messages the previous response set (see Flashes and FlashText) and expires
// the cookie in this response, so each message is shown once. A malformed
// or oversized cookie is discarded.
//
// The cookie is HttpOnly, Secure, SameSite=Lax and unsigned. It carries
// display text only, never a decision: a client that edits it changes only
// what its own browser displays, and the text is escaped when rendered. Do
// not put secrets or personal data in a flash message; it travels in the
// clear inside the TLS session and the browser's cookie store.
func Flash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &flashState{}
		if cookies := r.CookiesNamed(FlashCookieName); len(cookies) > 0 {
			if len(cookies) == 1 {
				state.incoming = decodeFlash(cookies[0].Value)
			}
			http.SetCookie(w, flashCookie("", true))
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), flashKey{}, state)))
	})
}

// Flashes returns the messages set by the previous response, in the order
// they were set.
func Flashes(ctx context.Context) []FlashMessage {
	state, ok := ctx.Value(flashKey{}).(*flashState)
	if !ok {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return slices.Clone(state.incoming)
}

// FlashText returns the text of the incoming message of kind, or "".
func FlashText(ctx context.Context, kind FlashKind) string {
	for _, message := range Flashes(ctx) {
		if message.Kind == kind {
			return message.Text
		}
	}
	return ""
}

// SetFlash stores a message for the next request, typically just before
// Redirect. Setting a kind twice keeps the last text. It must be called
// before the response is written, and fails for an invalid kind, invalid
// UTF-8, text above MaxFlashTextBytes, or a cookie above MaxFlashCookieBytes.
func SetFlash(w http.ResponseWriter, r *http.Request, kind FlashKind, text string) error {
	state, ok := r.Context().Value(flashKey{}).(*flashState)
	if !ok {
		return ErrFlashUnavailable
	}
	if !flashKindPattern.MatchString(string(kind)) {
		return fmt.Errorf("web: invalid flash kind %q", kind)
	}
	if !utf8.ValidString(text) || len(text) > MaxFlashTextBytes {
		return fmt.Errorf("web: flash %s text must be valid UTF-8 of at most %d bytes", kind, MaxFlashTextBytes)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	outgoing := slices.Clone(state.outgoing)
	if index := slices.IndexFunc(outgoing, func(m FlashMessage) bool { return m.Kind == kind }); index >= 0 {
		outgoing[index].Text = text
	} else {
		outgoing = append(outgoing, FlashMessage{Kind: kind, Text: text})
	}
	if len(outgoing) > MaxFlashMessages {
		return fmt.Errorf("web: at most %d flash messages per response", MaxFlashMessages)
	}
	encoded, err := encodeFlash(outgoing)
	if err != nil {
		return err
	}
	state.outgoing = outgoing
	removeFlashCookies(w.Header())
	http.SetCookie(w, flashCookie(encoded, false))
	return nil
}

func encodeFlash(messages []FlashMessage) (string, error) {
	wire := make([]wireFlash, 0, len(messages))
	for _, message := range messages {
		wire = append(wire, wireFlash{Kind: string(message.Kind), Text: message.Text})
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > MaxFlashCookieBytes {
		return "", fmt.Errorf("web: flash messages exceed %d encoded bytes", MaxFlashCookieBytes)
	}
	return encoded, nil
}

// decodeFlash returns nil for anything but a well-formed cookie this package
// could have written.
func decodeFlash(value string) []FlashMessage {
	if value == "" || len(value) > MaxFlashCookieBytes {
		return nil
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil
	}
	var wire []wireFlash
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire) == 0 || len(wire) > MaxFlashMessages {
		return nil
	}
	messages := make([]FlashMessage, 0, len(wire))
	for _, message := range wire {
		kind := FlashKind(message.Kind)
		if !flashKindPattern.MatchString(message.Kind) || !utf8.ValidString(message.Text) || len(message.Text) > MaxFlashTextBytes ||
			slices.ContainsFunc(messages, func(m FlashMessage) bool { return m.Kind == kind }) {
			return nil
		}
		messages = append(messages, FlashMessage{Kind: kind, Text: message.Text})
	}
	return messages
}

func flashCookie(value string, expire bool) *http.Cookie {
	// #nosec G124 -- Lax is the intentional SameSite policy; Secure and HttpOnly are set explicitly.
	cookie := &http.Cookie{
		Name:     FlashCookieName,
		Value:    value,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if expire {
		cookie.MaxAge = -1
		cookie.Expires = time.Unix(1, 0).UTC()
	}
	return cookie
}

// removeFlashCookies drops earlier flash Set-Cookie lines from this response,
// so the last SetFlash (or the expiry Flash added) is the only one sent.
func removeFlashCookies(header http.Header) {
	lines := header.Values("Set-Cookie")
	kept := lines[:0:0]
	for _, line := range lines {
		if !strings.HasPrefix(line, FlashCookieName+"=") {
			kept = append(kept, line)
		}
	}
	header.Del("Set-Cookie")
	for _, line := range kept {
		header.Add("Set-Cookie", line)
	}
}
