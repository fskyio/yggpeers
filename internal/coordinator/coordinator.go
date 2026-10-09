// Package coordinator schedules measurements without making peer connections.
package coordinator

import (
	"context"
	"log"
	"time"

	"yggpeers/internal/config"
	"yggpeers/internal/store"
)

// PollInterval keeps deadline processing and work pickup responsive without
// busy polling, including deployments using short measurement intervals.
func PollInterval(interval time.Duration) time.Duration {
	poll := interval / 10
	if poll > time.Second || poll <= 0 {
		poll = time.Second
	}
	if poll < 10*time.Millisecond {
		poll = 10 * time.Millisecond
	}
	return poll
}

func Run(ctx context.Context, cfg config.Config, st *store.Store) {
	advance := func() {
		changed, err := st.AdvanceProbeRounds(time.Now(), cfg.CheckInterval, cfg.RoundTimeout)
		if err != nil {
			log.Printf("probe rounds: %v", err)
			return
		}
		if changed {
			if err := st.PruneOldChecks(store.StatsWindowDays); err != nil {
				log.Printf("prune observations: %v", err)
			}
			st.RefreshCaches()
		}
	}
	advance()
	ticker := time.NewTicker(PollInterval(cfg.CheckInterval))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			advance()
		}
	}
}
