package proxy

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
)

// IsTransientProxyError reports whether a failed request looks like a local proxy
// (or its upstream) dropped the socket — Clash and similar tools routinely reset
// long downloads; that must not take down the serve process.
func IsTransientProxyError(err error) bool {
	for depth := 0; err != nil && depth < 5; depth++ {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return true
		}
		if errors.Is(err, io.ErrUnexpectedEOF) ||
			errors.Is(err, syscall.ECONNRESET) ||
			errors.Is(err, syscall.ECONNREFUSED) ||
			errors.Is(err, syscall.EPIPE) ||
			errors.Is(err, syscall.ETIMEDOUT) ||
			errors.Is(err, os.ErrDeadlineExceeded) {
			return true
		}
		msg := err.Error()
		if msg == "terminated" ||
			strings.Contains(strings.ToLower(msg), "other side closed") ||
			strings.Contains(msg, "does not match the HTTP/1.1 protocol") {
			return true
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			err = opErr.Err
			continue
		}
		unwrapped := errors.Unwrap(err)
		if unwrapped == nil {
			break
		}
		err = unwrapped
	}
	return false
}

// FormatFetchError returns a compact chain of messages for proxy/fetch failures.
func FormatFetchError(err error) string {
	if err == nil {
		return ""
	}
	parts := make([]string, 0, 5)
	for current := err; current != nil && len(parts) < 5; current = errors.Unwrap(current) {
		parts = append(parts, current.Error())
		if errors.Unwrap(current) == current {
			break
		}
	}
	if len(parts) == 0 {
		return err.Error()
	}
	return strings.Join(parts, " caused by ")
}
