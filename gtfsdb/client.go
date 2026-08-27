package gtfsdb

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// Client is the main entry point for the library
type Client struct {
	config        Config
	DB            *sql.DB
	Queries       *Queries
	importRuntime time.Duration
}

// NewClient creates a new Client with the provided configuration
func NewClient(config Config) (*Client, error) {
	db, err := createDB(config)
	if err != nil {
		return nil, fmt.Errorf("unable to create DB: %w", err)
	}
	slog.Default().Debug("successfully created DB")

	// Wrap DB for query interception (optional metrics).
	var dbtx DBTX = db
	if config.QueryMetricsRecorder != nil {
		wrapper := newMetricsWrapper(db)
		wrapper.queryMetrics = config.QueryMetricsRecorder
		dbtx = wrapper
	}
	queries := New(dbtx)

	client := &Client{
		config:  config,
		DB:      db,
		Queries: queries,
	}

	// Opportunistic maintenance, not a precondition for opening the database: a failure
	// here leaves the index as it was, and the next import rebuilds it outright.
	if err := client.backfillStopAgencyIndex(context.Background()); err != nil {
		slog.Default().Warn("unable to backfill stop agency index", "error", err)
	}

	return client, nil
}

// backfillStopAgencyIndex builds stop_agencies for a database imported before the table
// existed. An import is skipped when the feed hash is unchanged, so such a database would
// otherwise keep an empty index for as long as its feed stays the same.
func (c *Client) backfillStopAgencyIndex(ctx context.Context) error {
	if err := c.rebuildLegacyStopAgenciesTable(ctx); err != nil {
		return fmt.Errorf("failed to rebuild legacy stop agency table: %w", err)
	}

	var indexed, served int
	err := c.DB.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM stop_agencies),
			(SELECT EXISTS (SELECT 1 FROM stop_times))
	`).Scan(&indexed, &served)
	if err != nil {
		return fmt.Errorf("failed to check stop agency index: %w", err)
	}

	if indexed > 0 || served == 0 {
		return nil
	}

	slog.Default().Info("backfilling stop agency index for a database imported before it existed")
	return buildStopAgencyIndex(ctx, c.Queries)
}

// rebuildLegacyStopAgenciesTable drops and recreates stop_agencies if it still has the
// single-column primary key from before this table held one row per agency serving a
// stop. CREATE TABLE IF NOT EXISTS in schema.sql cannot reshape an already-existing
// table, so a database from an earlier revision of this branch is stuck on the old
// shape, with old data, otherwise.
func (c *Client) rebuildLegacyStopAgenciesTable(ctx context.Context) error {
	var pkColumns int
	err := c.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('stop_agencies') WHERE pk > 0`,
	).Scan(&pkColumns)
	if err != nil {
		return fmt.Errorf("failed to inspect stop_agencies schema: %w", err)
	}
	if pkColumns != 1 {
		return nil
	}

	slog.Default().Info("rebuilding stop_agencies: found single-column primary key from an earlier schema")
	if _, err := c.DB.ExecContext(ctx, "DROP TABLE stop_agencies"); err != nil {
		return fmt.Errorf("failed to drop legacy stop_agencies table: %w", err)
	}
	if _, err := c.DB.ExecContext(ctx, `
		CREATE TABLE stop_agencies (
			stop_id TEXT NOT NULL,
			agency_id TEXT NOT NULL,
			PRIMARY KEY (stop_id, agency_id),
			FOREIGN KEY (stop_id) REFERENCES stops (id),
			FOREIGN KEY (agency_id) REFERENCES agencies (id)
		) STRICT
	`); err != nil {
		return fmt.Errorf("failed to recreate stop_agencies table: %w", err)
	}
	if _, err := c.DB.ExecContext(ctx,
		"CREATE INDEX idx_stop_agencies_agency ON stop_agencies (agency_id, stop_id)",
	); err != nil {
		return fmt.Errorf("failed to recreate stop_agencies index: %w", err)
	}
	return nil
}

func (c *Client) Close() error {
	return c.DB.Close()
}
