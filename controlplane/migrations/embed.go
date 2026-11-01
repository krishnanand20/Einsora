// Package migrations embeds the SQL migrations applied by `tensorafi migrate` (goose format), one
// directory per database: postgres/ (Datacenter, larger Labs) and sqlite/ (Solo and Lab, ADR-0012).
//
// Add a migration: create NNNNN_description.sql in both directories, with -- +goose Up and
// -- +goose Down sections, describing the same change. Never edit a migration that has been applied
// anywhere; add a new one. TestDialectsMatch fails if the directories drift apart.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed postgres/*.sql sqlite/*.sql
var all embed.FS

// Postgres and SQLite are the migrations for each database, rooted at their directory.
var (
	Postgres = sub("postgres")
	SQLite   = sub("sqlite")
)

func sub(dir string) fs.FS {
	f, err := fs.Sub(all, dir)
	if err != nil {
		panic(err) // dir is a constant that exists in the embed above
	}
	return f
}
