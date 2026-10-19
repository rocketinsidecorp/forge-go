//go:build !linux && !darwin

package terminal

import "errors"

var errUnsupported = errors.New("terminal: hiding input is not supported on this platform")

type state struct{}

func getState(int) (*state, error) { return nil, errUnsupported }

func disableEcho(int, *state) error { return errUnsupported }

func restore(int, *state) error { return errUnsupported }
