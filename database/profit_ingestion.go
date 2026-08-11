package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (db *DB) recordProfitIngestionEvent(ctx context.Context, eventType, mode string, dropped int64, details string) error {
	if db == nil || db.conn == nil {
		return nil
	}
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return fmt.Errorf("profit ingestion event type is required")
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO profit_ingestion_events
			(event_type,mode,dropped_count,details,event_at) VALUES ($1,$2,$3,$4,$5)`,
			eventType, strings.TrimSpace(mode), dropped, strings.TrimSpace(details), db.timeArg(time.Now().UTC()))
		return err
	})
}

func (db *DB) validateProfitIngestionCompleteness(ctx context.Context, start, end time.Time) error {
	return db.validateProfitIngestionCompletenessQuery(ctx, db.conn, start, end)
}

func (db *DB) validateProfitIngestionCompletenessTx(ctx context.Context, tx *sql.Tx, start, end time.Time) error {
	return db.validateProfitIngestionCompletenessQuery(ctx, tx, start, end)
}

type profitIngestionQueryer interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

func (db *DB) validateProfitIngestionCompletenessQuery(ctx context.Context, queryer profitIngestionQueryer, start, end time.Time) error {
	mode := UsageLogModeFull
	err := queryer.QueryRowContext(ctx, `SELECT mode FROM profit_ingestion_events
		WHERE event_type='mode' AND event_at <= $1 ORDER BY event_at DESC,id DESC LIMIT 1`,
		db.timeArg(start.UTC())).Scan(&mode)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if NormalizeUsageLogMode(mode) != UsageLogModeFull {
		return fmt.Errorf("%w: usage logging was %s at range start", ErrProfitIngestionIncomplete, mode)
	}
	var badEvents int64
	if err := queryer.QueryRowContext(ctx, `SELECT COUNT(*) FROM profit_ingestion_events
		WHERE event_at >= $1 AND event_at < $2 AND
		(event_type IN ('drop','clear') OR (event_type='mode' AND mode<>'full'))`,
		db.timeArg(start.UTC()), db.timeArg(end.UTC())).Scan(&badEvents); err != nil {
		return err
	}
	if badEvents > 0 {
		return fmt.Errorf("%w: %d logging gap event(s) intersect the range", ErrProfitIngestionIncomplete, badEvents)
	}
	return nil
}
