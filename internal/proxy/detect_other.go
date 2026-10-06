//go:build !darwin

package proxy

// DetectSystemProxy is a no-op off macOS — there is no cheap system-proxy read.
func DetectSystemProxy() *SystemProxy {
	return nil
}
