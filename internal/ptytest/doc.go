// Package ptytest opens pseudo-terminals for tests that drive an interactive
// terminal: secret prompts, colored logs. It uses only the standard library's
// syscall package and exists on Linux; tests that use it carry a linux build
// constraint.
package ptytest
