// Package integration holds tests of core packages (encryption, migrate,
// sqldb, the root forge package, auth and its helpers) that need the SQLite
// driver. They live under the adapter that provides the driver so the core
// packages and their own tests import no third-party module (ADR 0053).
package integration
