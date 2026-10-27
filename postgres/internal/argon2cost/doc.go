// Package argon2cost tests, with the real golang.org/x/crypto/argon2, that
// auth refuses an Argon2id implementation that changes Forge's default cost
// (ADR 0053). Registration is once per process, so each case runs in a
// child process of the test binary that links no identity store.
package argon2cost
