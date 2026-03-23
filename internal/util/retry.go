package util

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/rs/zerolog/log"
)

// RetryConfig holds retry parameters.
type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// DefaultRetryConfig returns a default retry configuration.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Second,
		MaxDelay:    30 * time.Second,
	}
}

// Retry executes fn with exponential backoff.
func Retry(ctx context.Context, cfg RetryConfig, operation string, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s: context cancelled: %w", operation, err)
		}

		lastErr = fn()
		if lastErr == nil {
			return nil
		}

		if attempt < cfg.MaxAttempts-1 {
			delay := time.Duration(float64(cfg.BaseDelay) * math.Pow(2, float64(attempt)))
			if delay > cfg.MaxDelay {
				delay = cfg.MaxDelay
			}
			log.Warn().
				Err(lastErr).
				Str("operation", operation).
				Int("attempt", attempt+1).
				Dur("retry_in", delay).
				Msg("operation failed, retrying")

			select {
			case <-ctx.Done():
				return fmt.Errorf("%s: context cancelled during retry: %w", operation, ctx.Err())
			case <-time.After(delay):
			}
		}
	}
	return fmt.Errorf("%s: all %d attempts failed: %w", operation, cfg.MaxAttempts, lastErr)
}
