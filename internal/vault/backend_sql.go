package vault

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SQLBackend persists encrypted blobs in the credentials table. Writes
// happen through the existing migration-0002 schema: credentials(id PK,
// blob BLOB NOT NULL, ...). Soft-deleted rows (deleted_at IS NOT NULL)
// are hidden from Get/List so a deleted credential reads as ErrNotFound.
//
// Bind is engine-aware: Postgres uses $N placeholders natively; SQLite
// gets them rewritten to ?. The mapping mirrors what journal + credentials
// already do because the migration package guarantees one *sql.DB per
// engine.
type SQLBackend struct {
	db     *sql.DB
	engine SQLEngine
}

// SQLEngine names the SQL dialect for placeholder rewriting.
type SQLEngine string

const (
	SQLEngineSQLite   SQLEngine = "sqlite"
	SQLEnginePostgres SQLEngine = "postgres"
)

// NewSQLBackend wraps an open *sql.DB.
func NewSQLBackend(db *sql.DB, engine SQLEngine) *SQLBackend {
	return &SQLBackend{db: db, engine: engine}
}

// Put writes the encrypted blob. Creates the row if missing (caller is
// responsible for populating metadata via the credentials repo first).
func (b *SQLBackend) Put(ctx context.Context, id string, blob []byte) error {
	if id == "" {
		return errors.New("vault sql: id required")
	}
	// Try UPDATE first; if no row, INSERT a sentinel-name row. The CLI's
	// rotate path always has a credentials row already (created via the
	// repo) so the UPDATE branch is the production path.
	const upd = `UPDATE credentials SET blob = $1 WHERE id = $2 AND deleted_at IS NULL`
	res, err := b.db.ExecContext(ctx, b.bind(upd), blob, id)
	if err != nil {
		return fmt.Errorf("vault sql: update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return nil
	}
	const ins = `INSERT INTO credentials (id, name, service, blob) VALUES ($1, $2, 'misc', $3)`
	if _, err := b.db.ExecContext(ctx, b.bind(ins), id, id, blob); err != nil {
		return fmt.Errorf("vault sql: insert: %w", err)
	}
	return nil
}

// Get reads the blob.
func (b *SQLBackend) Get(ctx context.Context, id string) ([]byte, error) {
	const q = `SELECT blob FROM credentials WHERE id = $1 AND deleted_at IS NULL`
	var blob []byte
	if err := b.db.QueryRowContext(ctx, b.bind(q), id).Scan(&blob); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("vault sql: get: %w", err)
	}
	return blob, nil
}

// Delete soft-deletes the row by setting deleted_at = now. The blob is
// left intact so audit trails still work; readers filter on deleted_at.
func (b *SQLBackend) Delete(ctx context.Context, id string) error {
	const q = `UPDATE credentials SET deleted_at = $1 WHERE id = $2 AND deleted_at IS NULL`
	now := b.formatTime()
	res, err := b.db.ExecContext(ctx, b.bind(q), now, id)
	if err != nil {
		return fmt.Errorf("vault sql: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns every non-deleted credential id. Used for cache warmup.
func (b *SQLBackend) List(ctx context.Context) ([]string, error) {
	const q = `SELECT id FROM credentials WHERE deleted_at IS NULL ORDER BY id ASC`
	rows, err := b.db.QueryContext(ctx, b.bind(q))
	if err != nil {
		return nil, fmt.Errorf("vault sql: list: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (b *SQLBackend) bind(q string) string {
	if b.engine != SQLEngineSQLite {
		return q
	}
	out := make([]byte, 0, len(q))
	for i := 0; i < len(q); i++ {
		if q[i] == '$' && i+1 < len(q) && q[i+1] >= '0' && q[i+1] <= '9' {
			out = append(out, '?')
			i++
			for i+1 < len(q) && q[i+1] >= '0' && q[i+1] <= '9' {
				i++
			}
			continue
		}
		out = append(out, q[i])
	}
	return string(out)
}

func (b *SQLBackend) formatTime() any {
	if b.engine == SQLEngineSQLite {
		// ISO-8601 UTC matches the strftime default the schema uses.
		return nowISO()
	}
	return nowUTC()
}
