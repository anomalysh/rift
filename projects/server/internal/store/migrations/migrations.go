// Package migrations holds the embedded SQL schema migrations that the
// postgres adapter applies at boot.
package migrations

import "embed"

// FS carries every NNNN_*.sql migration. The runner reads and orders them by
// the numeric prefix of each filename.
//
//go:embed *.sql
var FS embed.FS
