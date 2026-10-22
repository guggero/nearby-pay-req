package nearby

import (
	"sync"
	"time"
)

// budget is a sliding-window count shared by every find of a Manager: at
// most max events within any window. The Manager keeps two, both bounds on
// a man in the middle (see Params.MaxOffers and
// Params.MaxCodeAttemptsPerWindow): the requests a payer holds for
// confirmation, each a code an attacker may try to make a payee show, and
// the sessions in which it revealed its nonce, each a chance for a rogue
// payee to see a code it likes. They belong to the Manager rather than to
// one find, so restarting a search does not reset them.
type budget struct {
	max    int
	window time.Duration

	mu    sync.Mutex
	times []time.Time
}

// nextFree returns when the budget has room for one more event: now, or the
// moment the oldest event in the window ages out.
func (b *budget) nextFree(now time.Time) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.times = pruneBefore(b.times, now.Add(-b.window))
	if len(b.times) < b.max {
		return now
	}

	return b.times[0].Add(b.window)
}

// take records an event at now.
func (b *budget) take(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.times = append(pruneBefore(b.times, now.Add(-b.window)), now)
}

// recent counts the events within the last d before now, which may be
// shorter than the window.
func (b *budget) recent(now time.Time, d time.Duration) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(pruneBefore(b.times, now.Add(-d)))
}
