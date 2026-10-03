// Package pgdomino is a domino engine that stores rules in Postgres, in tables
// prefixed with domino_.
package pgdomino

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewmueller/domino"
	"github.com/matthewmueller/domino/pgdomino/internal/migrate"
)

// Dial connects to Postgres, creates or upgrades domino's tables, and returns
// an engine that stores rules there
func Dial(ctx context.Context, log *slog.Logger, url string) (*Engine, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("pgdomino: unable to connect: %w", err)
	}
	if err := migrate.Up(log, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgdomino: unable to migrate: %w", err)
	}
	engine := domino.New()
	engine.Store = &store{pool}
	return &Engine{engine, pool}, nil
}

// Engine is a domino.Engine whose rules are stored in Postgres. Everything but
// Close comes from the embedded engine.
type Engine struct {
	*domino.Engine
	pool *pgxpool.Pool
}

// Close closes the connection pool
func (e *Engine) Close() {
	e.pool.Close()
}
