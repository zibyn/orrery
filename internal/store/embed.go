package store

import "embed"

// Migrations is embedded so the server binary can migrate without shipping SQL
// files separately (ADR-0014).
//
//go:embed migrations/*.sql
var Migrations embed.FS
