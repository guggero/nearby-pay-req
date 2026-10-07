package nearby

import (
	"sync"
	"time"
)

// offerBudget bounds how many received requests a payer may hold for
// confirmation at once: at most max within any window, across every find of
// a Manager. Each one is a code an attacker may try to make a payee show,
// so the budget, not the number of sharers in range, sets the odds of a
// man in the middle (see Params.MaxOffers).
type offerBudget struct {
	max    int
	window time.Duration

	mu    sync.Mutex
	times []time.Time
}

// nextFree returns when the budget has room for one more offer: now, or the
// moment the oldest offer in the window ages out.
func (b *offerBudget) nextFree(now time.Time) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.times = pruneBefore(b.times, now.Add(-b.window))
	if len(b.times) < b.max {
		return now
	}

	return b.times[0].Add(b.window)
}

// take records an offer handed out at now.
func (b *offerBudget) take(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.times = append(pruneBefore(b.times, now.Add(-b.window)), now)
}
