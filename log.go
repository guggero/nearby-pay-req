package nearby

import "github.com/btcsuite/btclog/v2"

const (
	// Subsystem defines the logging code for this subsystem.
	Subsystem = "NRBY"
)

var (
	// log is a logger that is initialized with no output filters. The
	// package performs no logging until the caller wires a logger via
	// UseLogger.
	log = btclog.Disabled
)

// DisableLog disables all library log output. Logging output is disabled by
// default until UseLogger is called.
func DisableLog() {
	UseLogger(btclog.Disabled)
}

// UseLogger uses a specified Logger to output package logging info.
func UseLogger(logger btclog.Logger) {
	log = logger
}
