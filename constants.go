package udp

import (
	"errors"
	"time"
)

const (
	antsPoolExpiryDuration = 120 * time.Second
	antsPoolNonblocking    = true
	antsPoolPrealloc       = false
	antsPoolDefaultSize    = 1024 * 1024

	mtuLimit = 1500
)

const (
	serverStarting = iota
	serverStarted
	serverStopping
	serverStopped
)

var (
	// Basic udp server errors
	ErrServerNotStart       = errors.New("server not start")
	ErrServerAlreadyStarted = errors.New("server already started")
)
