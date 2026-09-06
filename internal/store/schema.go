package store

import (
	"context"
	_ "embed"
	"fmt"
)

//go:embed schema.sql
var schema string

func (d *Store) initializeSchema(ctx context.Context) error {
	if _, err := d.db.Exec(ctx, schema); err != nil {
		return fmt.Errorf("initialize PostgreSQL schema: %w", err)
	}
	return nil
}
