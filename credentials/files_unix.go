//go:build unix

package credentials

import "syscall"

// openNonblock keeps opening a FIFO from blocking (see openNonblocking).
const openNonblock = syscall.O_NONBLOCK
