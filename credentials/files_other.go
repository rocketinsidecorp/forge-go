//go:build !unix

package credentials

// openNonblock is not needed where there are no FIFOs in the file system.
const openNonblock = 0
