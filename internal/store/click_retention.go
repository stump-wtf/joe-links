// Governing: SPEC-0021 REQ "Click Retention", ADR-0021, ADR-0002
package store

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
)

// PruneBatchSize is the production per-statement victim cap for retention
// pruning: at most this many rows are deleted per statement, iterating until
// done, so a large first prune cannot hold a long transaction on sqlite3.
// Governing: SPEC-0021 REQ "Click Retention" — bounded batches
const PruneBatchSize = 10000

// PruneClicksBefore deletes link_clicks rows with clicked_at older than
// cutoff, in bounded batches of at most batchSize rows per statement,
// iterating until no victims remain. It returns the total number of rows
// deleted.
//
// The batching pattern is pinned by SPEC-0021: each batch keyset-selects the
// victim ids (SELECT id … ORDER BY clicked_at, id LIMIT n) and then deletes
// them by id — DELETE … LIMIT is not used in any form, because postgres does
// not support it, sqlite3 requires a non-default build tag, and mysql rejects
// the same-table subselect workaround; select-ids-then-delete is the only
// shape portable across all three drivers (ADR-0002).
// Governing: SPEC-0021 REQ "Click Retention", ADR-0021 (e), ADR-0002
func (s *ClickStore) PruneClicksBefore(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = PruneBatchSize
	}

	var total int64
	for {
		var ids []string
		err := s.db.SelectContext(ctx, &ids, s.q(`
			SELECT id FROM link_clicks
			WHERE clicked_at < ?
			ORDER BY clicked_at, id
			LIMIT ?
		`), cutoff, batchSize)
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}

		query, args, err := sqlx.In(`DELETE FROM link_clicks WHERE id IN (?)`, ids)
		if err != nil {
			return total, err
		}
		res, err := s.db.ExecContext(ctx, s.db.Rebind(query), args...)
		if err != nil {
			return total, err
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		} else {
			total += int64(len(ids))
		}

		// A short batch means the victim set is exhausted — no need for one
		// more empty round trip.
		if len(ids) < batchSize {
			return total, nil
		}
	}
}
