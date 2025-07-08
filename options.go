package udp

import (
	"log"
	"time"

	"github.com/panjf2000/ants/v2"
)

type Option func(opts *Options)

type Options struct {
	pool     *ants.Pool
	poolSize int // sizeof ants.Pool
	logger   *log.Logger
	tick     time.Duration
}

func WithWorkerPoolSize(poolSize int) Option {
	return func(opts *Options) {
		opts.poolSize = poolSize
	}
}

func WithLogger(logger *log.Logger) Option {
	return func(opts *Options) {
		opts.logger = logger
	}
}

func WithTickInterval(tick time.Duration) Option {
	return func(opts *Options) {
		opts.tick = tick
	}
}

// This is a temporary solution, this function may be
// removed in the future.
func WithWorkerPool(pool *ants.Pool) Option {
	return func(opts *Options) {
		opts.pool = pool
	}
}
