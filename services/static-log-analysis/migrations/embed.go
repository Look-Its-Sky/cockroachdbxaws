// Package migrations exposes the reviewed CockroachDB migration files to the
// persistence adapter. Keeping SQL in files makes schema changes independently
// reviewable while embedding them keeps deployment independent of a working
// directory.
package migrations

import "embed"

// Files contains every versioned SQL migration.
//
//go:embed *.sql
var Files embed.FS
