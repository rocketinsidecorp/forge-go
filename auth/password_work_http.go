package auth

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

// setPasswordWorkRetryAfter sets Retry-After when err is a saturated
// PasswordWorkLimiter, whose 503 the caller then writes.
func setPasswordWorkRetryAfter(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrPasswordHashingUnavailable) {
		w.Header().Set("Retry-After", strconv.Itoa(int(PasswordWorkRetryAfter/time.Second)))
	}
}
