// Package ptytest opens pseudo-terminals for tests that drive an interactive
// terminal: secret prompts, colored logs. It uses only the standard library's
// syscall package and exists on Linux and macOS; tests that use it carry a
// matching build constraint.
package ptytest
