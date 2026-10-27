package authhttp

import (
	"errors"
	"github.com/rocketinsidecorp/forge-go/auth"
	"net/http"
	"strconv"
	"time"
)

// setPasswordWorkRetryAfter sets Retry-After when err is a saturated
// PasswordWorkLimiter, whose 503 the caller then writes.
func setPasswordWorkRetryAfter(w http.ResponseWriter, err error) {
	if errors.Is(err, auth.ErrPasswordHashingUnavailable) {
		w.Header().Set("Retry-After", strconv.Itoa(int(auth.PasswordWorkRetryAfter/time.Second)))
	}
}
