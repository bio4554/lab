// Package migrations embeds the SQL migration files for both goose
// streams so the daemons can self-check schema currency at startup.
package migrations

import "embed"

// Lab holds the lab-schema migration stream (lab/*.sql).
//
//go:embed lab/*.sql
var Lab embed.FS

// Kbase holds the kbase-schema migration stream (kbase/*.sql).
//
//go:embed kbase/*.sql
var Kbase embed.FS
