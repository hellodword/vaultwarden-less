package worker

import "time"

const (
	ListenAddress = "0.0.0.0:9090"

	quietWindow           = 30 * time.Second
	maximumQuietWindow    = 2 * time.Minute
	minimumCheckInterval  = 5 * time.Minute
	fallbackCheckInterval = 12 * time.Hour
	consistencyRetry      = 30 * time.Second
	failureRetry          = 5 * time.Minute
	degradedAfter         = 30 * time.Minute
	targetTimeout         = 2 * time.Hour
	eventMaxBytes         = 1024

	httpReadHeaderTimeout = 5 * time.Second
	httpReadTimeout       = 10 * time.Second
	httpWriteTimeout      = 10 * time.Second
	httpIdleTimeout       = 30 * time.Second
)
