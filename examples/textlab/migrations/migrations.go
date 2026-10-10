// Package migrations holds Textlab's database migrations.
package migrations

import "embed"

// Files are applied after C4Stack's own migrations.
//
//go:embed *
var Files embed.FS
