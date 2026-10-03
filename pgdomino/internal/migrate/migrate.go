// Package migrate holds domino's Postgres migrations. Tables are prefixed with
// domino_, including the version table, so they sit beside the host's tables.
package migrate

import (
	"embed"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewmueller/migrate/pgxmigrate"
)

//go:embed *.sql
var fsys embed.FS

// Up applies any pending migrations
func Up(log *slog.Logger, pool *pgxpool.Pool) error {
	return pgxmigrate.Up(log, pool, fsys, "domino_migrate")
}

// Down rolls back every migration
func Down(log *slog.Logger, pool *pgxpool.Pool) error {
	return pgxmigrate.Down(log, pool, fsys, "domino_migrate")
}
