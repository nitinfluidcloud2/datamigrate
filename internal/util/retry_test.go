package util

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetrySuccess(t *testing.T) {
	attempts := 0
	err := Retry(context.Background(), RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    100 * time.Millisecond,
	}, "test", func() error {
		attempts++
		if attempts < 3 {
			return errors.New("fail")
		}
		return nil
	})

	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestRetryAllFail(t *testing.T) {
	err := Retry(context.Background(), RetryConfig{
		MaxAttempts: 2,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    100 * time.Millisecond,
	}, "test", func() error {
		return errors.New("always fail")
	})

	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRetryCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Retry(ctx, DefaultRetryConfig(), "test", func() error {
		return errors.New("fail")
	})

	if err == nil {
		t.Fatal("expected error")
	}
}
