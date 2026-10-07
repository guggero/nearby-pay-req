package nearby

import (
	"time"
)

// Default parameter values.
const (
	defaultSessionTimeout       = 10 * time.Second
	defaultConnectTimeout       = 5 * time.Second
	defaultCollectWindow        = 800 * time.Millisecond
	defaultFindGiveUpAfter      = 20 * time.Second
	defaultRSSIFloor            = -75
	defaultRSSISmoothing        = 0.3
	defaultMinSessionInterval   = time.Second
	defaultMaxSessionsPerWindow = 30
	defaultSessionRateWindow    = time.Minute
	defaultSuspiciousThreshold  = 3
	defaultSuspiciousWindow     = time.Minute
	defaultBusyRetryDelay       = 1500 * time.Millisecond
	defaultMaxBusyRetries       = 3
	defaultChosenRetention      = 2 * time.Minute
	defaultChooseAttempts       = 3
	defaultChooseRetryDelay     = time.Second
	defaultScanStopDebounce     = 2 * time.Second
	defaultEventQueueLen        = 256
	defaultFirstMessageTimeout  = 3 * time.Second
	defaultMaxCentrals          = 8
)

// Params are the timing, selection and rate-limiting knobs of the Manager.
// The defaults suit two phones held close together in a quiet room; an app
// can tune them for its environment, for example a busy shop counter with
// many phones in range, or hardware with slower radios. A zero field means
// the default (see DefaultParams); negative values of durations and counts
// also fall back to the default.
type Params struct {
	// SessionTimeout bounds one whole exchange, from the payer's first
	// message to its acknowledgement, on both sides.
	SessionTimeout time.Duration

	// ConnectTimeout bounds connecting, service discovery and
	// subscription on the payer.
	ConnectTimeout time.Duration

	// CollectWindow is how long the payer keeps collecting
	// advertisements after a first sighting before it picks the closest
	// sharer. Longer windows give steadier RSSI averages among many
	// sharers; shorter ones answer faster.
	CollectWindow time.Duration

	// FindGiveUpAfter is when a find without any completed session
	// reports a FailureTimeout. It keeps scanning afterwards; the UI
	// only changes its wording.
	FindGiveUpAfter time.Duration

	// RSSIFloor is the weakest smoothed RSSI (dBm) a sharer may have to
	// be picked. Below it the payer reports TooFar. Set it to -127 to
	// pick sharers at any distance.
	RSSIFloor int

	// RSSISmoothing is the weight of a new RSSI sample in the moving
	// average, in (0, 1]. Lower values damp the large swings phone RSSI
	// shows; 1 disables smoothing. Values outside (0, 1] mean the
	// default.
	RSSISmoothing float64

	// MinSessionInterval is the shortest gap between two sessions a
	// payee accepts.
	MinSessionInterval time.Duration

	// MaxSessionsPerWindow caps payee sessions per SessionRateWindow.
	// Together with MinSessionInterval it throttles anyone trying
	// sessions until a code matches; a room with many payers may need
	// a higher cap.
	MaxSessionsPerWindow int

	// SessionRateWindow is the rolling window of MaxSessionsPerWindow.
	SessionRateWindow time.Duration

	// SuspiciousThreshold is how many sessions that failed after showing
	// a code, within SuspiciousWindow, raise a SuspiciousActivity.
	SuspiciousThreshold int

	// SuspiciousWindow is the rolling window of SuspiciousThreshold.
	SuspiciousWindow time.Duration

	// BusyRetryDelay is how long a payer waits before trying a sharer
	// again that answered busy (serving another payer or rate limiting).
	// In a room with several payers this is what lets everyone get the
	// request in turn.
	BusyRetryDelay time.Duration

	// MaxBusyRetries is how often a payer retries a busy sharer within
	// one find before skipping it for the rest of the find.
	MaxBusyRetries int

	// ChosenRetention is how long a payee remembers a delivered session,
	// so a payer whose user picks that request later can still tell the
	// payee with Manager.Choose.
	ChosenRetention time.Duration

	// ChooseAttempts is how often Manager.Choose tries to reach the
	// payee before giving up.
	ChooseAttempts int

	// ChooseRetryDelay is the pause between two Choose attempts.
	ChooseRetryDelay time.Duration

	// ScanStopDebounce delays stopping a scan, so leaving and quickly
	// re-entering a send screen reuses the running scan instead of
	// spending one of the five scan starts Android allows per 30 s.
	ScanStopDebounce time.Duration

	// EventQueueLen buffers radio callbacks per share or find. Callbacks
	// never block, so an overflowing queue drops events; the affected
	// session then fails and the peer can retry.
	EventQueueLen int

	// FirstMessageTimeout is how long a payee waits, from a payer's
	// subscription, for its first complete message before dropping it,
	// so a silent or trickling payer cannot hold a connection and a
	// partly received message for ever.
	FirstMessageTimeout time.Duration

	// MaxCentrals caps how many payers may be connected to a payee
	// without a session at once. Each may hold a partly received
	// message of up to 12288 bytes; payers beyond the cap are dropped as
	// they subscribe.
	MaxCentrals int
}

// DefaultParams returns the parameters the Manager uses unless told
// otherwise.
func DefaultParams() Params {
	return Params{
		SessionTimeout:       defaultSessionTimeout,
		ConnectTimeout:       defaultConnectTimeout,
		CollectWindow:        defaultCollectWindow,
		FindGiveUpAfter:      defaultFindGiveUpAfter,
		RSSIFloor:            defaultRSSIFloor,
		RSSISmoothing:        defaultRSSISmoothing,
		MinSessionInterval:   defaultMinSessionInterval,
		MaxSessionsPerWindow: defaultMaxSessionsPerWindow,
		SessionRateWindow:    defaultSessionRateWindow,
		SuspiciousThreshold:  defaultSuspiciousThreshold,
		SuspiciousWindow:     defaultSuspiciousWindow,
		BusyRetryDelay:       defaultBusyRetryDelay,
		MaxBusyRetries:       defaultMaxBusyRetries,
		ChosenRetention:      defaultChosenRetention,
		ChooseAttempts:       defaultChooseAttempts,
		ChooseRetryDelay:     defaultChooseRetryDelay,
		ScanStopDebounce:     defaultScanStopDebounce,
		EventQueueLen:        defaultEventQueueLen,
		FirstMessageTimeout:  defaultFirstMessageTimeout,
		MaxCentrals:          defaultMaxCentrals,
	}
}

// withDefaults returns p with every unset or invalid field replaced by its
// default.
func (p Params) withDefaults() Params {
	d := DefaultParams()

	duration := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	count := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}

	duration(&p.SessionTimeout, d.SessionTimeout)
	duration(&p.ConnectTimeout, d.ConnectTimeout)
	duration(&p.CollectWindow, d.CollectWindow)
	duration(&p.FindGiveUpAfter, d.FindGiveUpAfter)
	duration(&p.MinSessionInterval, d.MinSessionInterval)
	duration(&p.SessionRateWindow, d.SessionRateWindow)
	duration(&p.SuspiciousWindow, d.SuspiciousWindow)
	duration(&p.BusyRetryDelay, d.BusyRetryDelay)
	duration(&p.ChosenRetention, d.ChosenRetention)
	duration(&p.ChooseRetryDelay, d.ChooseRetryDelay)
	duration(&p.ScanStopDebounce, d.ScanStopDebounce)
	duration(&p.FirstMessageTimeout, d.FirstMessageTimeout)
	count(&p.MaxSessionsPerWindow, d.MaxSessionsPerWindow)
	count(&p.SuspiciousThreshold, d.SuspiciousThreshold)
	count(&p.MaxBusyRetries, d.MaxBusyRetries)
	count(&p.ChooseAttempts, d.ChooseAttempts)
	count(&p.EventQueueLen, d.EventQueueLen)
	count(&p.MaxCentrals, d.MaxCentrals)

	// RSSI is negative by nature, so only the unset zero means the
	// default.
	if p.RSSIFloor == 0 {
		p.RSSIFloor = d.RSSIFloor
	}
	if p.RSSISmoothing <= 0 || p.RSSISmoothing > 1 {
		p.RSSISmoothing = d.RSSISmoothing
	}

	return p
}
