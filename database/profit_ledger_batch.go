package database

import (
	"context"
	"runtime"
	"time"
)

const MaxProfitLedgerRefreshRequestLimit = 20_000

// RefreshProfitDailyLedgerBatched processes a larger finite catch-up request while
// preserving the 100-row transaction boundary used by RefreshProfitDailyLedger.
// This avoids thousands of HTTP round trips for historical imports without ever
// holding one SQLite write transaction across the full requested range.
func (db *DB) RefreshProfitDailyLedgerBatched(ctx context.Context, limit int) (ProfitLedgerRefreshResult, error) {
	if limit <= 0 {
		limit = DefaultProfitLedgerRefreshLimit
	}
	if limit > MaxProfitLedgerRefreshRequestLimit {
		limit = MaxProfitLedgerRefreshRequestLimit
	}

	var result ProfitLedgerRefreshResult
	for result.ProcessedLogs < int64(limit) {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		batchLimit := limit - int(result.ProcessedLogs)
		if batchLimit > MaxProfitLedgerRefreshLimit {
			batchLimit = MaxProfitLedgerRefreshLimit
		}
		batch, err := db.RefreshProfitDailyLedger(ctx, batchLimit)
		if err != nil {
			return result, err
		}
		if result.HighWaterID == 0 {
			result.HighWaterID = batch.HighWaterID
		}
		result.ProcessedLogs += batch.ProcessedLogs
		result.AggregatedLogs += batch.AggregatedLogs
		result.CheckpointID = batch.CheckpointID
		result.RemainingLogs = result.HighWaterID - result.CheckpointID
		if result.RemainingLogs < 0 {
			result.RemainingLogs = 0
		}
		result.CaughtUp = result.CheckpointID >= result.HighWaterID
		result.UpdatedAt = time.Now().Format(time.RFC3339)
		if batch.ProcessedLogs == 0 || result.CaughtUp {
			break
		}
		runtime.Gosched()
	}
	return result, nil
}
