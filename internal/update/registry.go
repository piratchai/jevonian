package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type Registry struct {
	HTTP HTTPClient
	URL  string
}

const (
	defaultRegistryTimeout = 30 * time.Second
	minRegistryTimeout     = time.Second
	maxRegistryTimeout     = 5 * time.Minute
)

// RegistryTimeout is the per-request registry timeout: JEVONIAN_REGISTRY_TIMEOUT_MS
// clamped to [1s, 5m], default 30s. src/updates.ts registryTimeoutMs.
func RegistryTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("JEVONIAN_REGISTRY_TIMEOUT_MS"))
	if raw == "" {
		return defaultRegistryTimeout
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return defaultRegistryTimeout
	}
	d := time.Duration(ms) * time.Millisecond
	return min(maxRegistryTimeout, max(minRegistryTimeout, d))
}

func (r Registry) FetchLatest(ctx context.Context) (string, error) {
	address := r.URL
	if address == "" {
		address = "https://registry.npmjs.org/jevonian/latest"
	}
	timeout := RegistryTimeout()
	response, err := request(ctx, r.HTTP, http.MethodGet, address, timeout)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("registry returned %d", response.StatusCode)
	}
	var doc struct {
		Version string `json:"version"`
		Dist    struct {
			Tarball string `json:"tarball"`
		} `json:"dist"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&doc); err != nil || !validVersion(doc.Version) {
		return "", fmt.Errorf("registry response did not contain a valid version")
	}
	tarball := doc.Dist.Tarball
	if tarball == "" {
		tarball = "https://registry.npmjs.org/jevonian/-/jevonian-" + doc.Version + ".tgz"
	}
	probe, err := request(ctx, r.HTTP, http.MethodHead, tarball, timeout)
	if err != nil {
		return "", err
	}
	defer probe.Body.Close()
	if probe.StatusCode < 200 || probe.StatusCode >= 300 {
		return "", fmt.Errorf("jevonian@%s is listed on the registry but the package tarball is not available yet (%d)", doc.Version, probe.StatusCode)
	}
	return doc.Version, nil
}

// Requests reject non-HTTP URLs and credentials. Caller contexts and hard
// per-request deadlines apply even when the injected client has no timeout.
func request(ctx context.Context, client HTTPClient, method, address string, timeout time.Duration) (*http.Response, error) {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return nil, fmt.Errorf("invalid update URL")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(ctx, method, address, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "jevonian-update-check")
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	response.Body = &cancelBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { defer b.cancel(); return b.ReadCloser.Close() }
