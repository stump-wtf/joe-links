// Package retention implements the opt-in click-retention pruner: a
// background goroutine started by `joe-links serve` (the click-writer /
// health-checker pattern — ADR-0016, ADR-0020) that periodically deletes
// link_clicks rows older than the configured horizon in bounded portable
// batches. Retention is off by default: with Days 0 (JOE_CLICK_RETENTION
// unset) Run returns immediately and no click row is ever deleted.
//
// Governing: SPEC-0021 REQ "Click Retention", ADR-0021 (e)
package retention

import (
	"context"
	"log"
	"time"

	"github.com/joestump/joe-links/internal/metrics"
	"github.com/joestump/joe-links/internal/store"
)

// Config carries the viper-loaded retention settings (JOE_ prefix).
// Governing: SPEC-0021 REQ "Click Retention"
type Config struct {
	// Days is the retention horizon in days (JOE_CLICK_RETENTION). 0 disables
	// retention entirely; config.Load enforces the non-negative and ≥90-day
	// constraints before a Pruner is ever built.
	Days int
	// Interval is the wake-up cadence between prune runs after the startup
	// run. SPEC-0021 requires a run at startup and at least every 24 hours.
	Interval time.Duration
}

// Pruner deletes click rows older than the retention horizon.
type Pruner struct {
	clicks *store.ClickStore
	cfg    Config

	// batchSize is store.PruneBatchSize in production; tests shrink it to
	// exercise multi-batch prunes without seeding tens of thousands of rows.
	batchSize int
}

// New builds a Pruner. The pruner assumes a single running joe-links instance
// (SPEC-0021): multi-replica deployments must leave retention disabled on all
// but one instance.
func New(clicks *store.ClickStore, cfg Config) *Pruner {
	if cfg.Interval <= 0 {
		cfg.Interval = 24 * time.Hour
	}
	return &Pruner{clicks: clicks, cfg: cfg, batchSize: store.PruneBatchSize}
}

// Run prunes at startup and then every Interval until ctx is cancelled. When
// retention is disabled (Days 0) it returns immediately — the application
// never deletes click data on deployments that have not explicitly opted in.
//
// The startup log line is normative: pruning is irreversible deletion of
// source-of-truth rows, so an operator must be able to see from the log that
// retention is active and what its horizon is.
// Governing: SPEC-0021 REQ "Click Retention" — scenarios "Default is no
// deletion", "Startup surfacing"
func (p *Pruner) Run(ctx context.Context) {
	if p.cfg.Days <= 0 {
		return
	}
	log.Printf("click retention enabled (%d-day horizon): rows older than the horizon are pruned periodically and are unrecoverable", p.cfg.Days)

	p.runOnce(ctx)
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.runOnce(ctx)
		}
	}
}

// runOnce executes one prune pass: delete rows with clicked_at older than
// now − Days in bounded batches, log the count, and increment the
// joelinks_clicks_pruned_total counter.
// Governing: SPEC-0021 REQ "Click Retention" — scenarios "Opt-in pruning",
// "Batched deletes"
func (p *Pruner) runOnce(ctx context.Context) {
	cutoff := time.Now().UTC().AddDate(0, 0, -p.cfg.Days)
	pruned, err := p.clicks.PruneClicksBefore(ctx, cutoff, p.batchSize)
	if pruned > 0 {
		metrics.ClicksPrunedTotal.Add(float64(pruned))
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("click retention: prune failed after %d rows: %v", pruned, err)
		}
		return
	}
	log.Printf("click retention: pruned %d click rows older than %s (%d-day horizon)", pruned, cutoff.Format(time.RFC3339), p.cfg.Days)
}
