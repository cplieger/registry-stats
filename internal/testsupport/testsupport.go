// Package testsupport provides test helpers shared across registry-stats test packages.
package testsupport

import "log/slog"

// QuietLogger returns a logger that discards output, suitable for
// tests that don't assert on log lines.
func QuietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
