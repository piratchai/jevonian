package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestIsRetryableStatus(t *testing.T) {
	for _, status := range []int{408, 500, 502, 503, 504} {
		if !IsRetryableStatus(status) {
			t.Fatalf("expected %d retryable", status)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 422, 429} {
		if IsRetryableStatus(status) {
			t.Fatalf("did not expect %d retryable", status)
		}
	}
}

func TestIsRetryableError(t *testing.T) {
	if !IsRetryableError(&TimeoutError{Phase: PhaseHeaders, MS: 1000}) {
		t.Fatal("timeout should be retryable")
	}
	if !IsRetryableError(syscall.ECONNRESET) {
		t.Fatal("ECONNRESET should be retryable")
	}
	if !IsRetryableError(io.EOF) {
		t.Fatal("EOF from a dropped connection should be retryable")
	}
	if !IsRetryableError(&net.OpError{Err: syscall.ECONNREFUSED}) {
		t.Fatal("ECONNREFUSED should be retryable")
	}
	if IsRetryableError(context.Canceled) {
		t.Fatal("caller cancel must not be retryable")
	}
	if IsRetryableError(errors.New("Invalid JSON body")) {
		t.Fatal("ordinary errors must not be retryable")
	}
}

func TestConfiguredSameHostRetries(t *testing.T) {
	t.Setenv("JEVONIAN_SAME_HOST_RETRIES", "")
	_ = os.Unsetenv("JEVONIAN_SAME_HOST_RETRIES")
	if ConfiguredSameHostRetries() != 1 {
		t.Fatalf("default = %d", ConfiguredSameHostRetries())
	}
	t.Setenv("JEVONIAN_SAME_HOST_RETRIES", "0")
	if ConfiguredSameHostRetries() != 0 {
		t.Fatalf("got %d", ConfiguredSameHostRetries())
	}
	t.Setenv("JEVONIAN_SAME_HOST_RETRIES", "99")
	if ConfiguredSameHostRetries() != 5 {
		t.Fatalf("cap = %d", ConfiguredSameHostRetries())
	}
}

func TestWithRetrySameHostBudget(t *testing.T) {
	hits := 0
	_, err := WithRetry(func() (int, error) {
		hits++
		return 0, syscall.ECONNRESET
	}, RetryOptions[int]{
		Attempts: 2, // 1 same-host retry
		Sleep:    func(time.Duration) {},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}
}

func TestWithRetryTransientStatus(t *testing.T) {
	hits := 0
	out, err := WithRetry(func() (int, error) {
		hits++
		if hits < 2 {
			return 502, nil
		}
		return 200, nil
	}, RetryOptions[int]{
		Attempts: 3,
		RetryWhen: func(status int) *RetryFailure {
			if IsRetryableStatus(status) {
				return &RetryFailure{Status: status}
			}
			return nil
		},
		Sleep: func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != 200 || hits != 2 {
		t.Fatalf("out=%d hits=%d", out, hits)
	}
}
