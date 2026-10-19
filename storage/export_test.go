package storage

import "time"

// SetClock replaces the signer's clock in tests.
func SetClock(s *Signer, now func() time.Time) { s.now = now }
