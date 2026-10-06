package freebuff

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/provider/multiacct"
)

// Credentials is the sign-in file Jevonian writes after a device-code login.
// The shape matches the one the community Freebuff tooling uses, so a file
// produced by those scripts can be pointed at directly.
type Credentials struct {
	AuthToken string `json:"authToken"`
	UserID    string `json:"userId,omitempty"`
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
}

// ErrNoCredentials is returned when no token can be found.
var ErrNoCredentials = errors.New("Freebuff credentials not found. Run `jevonian add freebuff-subscription` to sign in, set FREEBUFF_AUTH_TOKEN, or point JEVONIAN_FREEBUFF_AUTH at a credentials file.")

// SessionPath is Jevonian's own sign-in file. JEVONIAN_FREEBUFF_AUTH wins,
// then a login's credentialsPath, then $XDG_CONFIG_HOME/jevonian/freebuff.json.
func SessionPath(login *multiacct.Login) string {
	if v := strings.TrimSpace(os.Getenv("JEVONIAN_FREEBUFF_AUTH")); v != "" {
		return v
	}
	if login != nil && login.CredentialsPath != "" {
		return login.CredentialsPath
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "jevonian", "freebuff.json")
}

// ReadToken resolves the auth token: FREEBUFF_AUTH_TOKEN first (unless the
// login names its own file), then the sign-in file.
func ReadToken(login *multiacct.Login) (string, error) {
	explicit := strings.TrimSpace(os.Getenv("JEVONIAN_FREEBUFF_AUTH")) != "" ||
		(login != nil && login.CredentialsPath != "")
	if !explicit {
		if v := strings.TrimSpace(os.Getenv("FREEBUFF_AUTH_TOKEN")); v != "" {
			return v, nil
		}
	}
	creds, err := ReadCredentials(SessionPath(login))
	if err != nil {
		return "", err
	}
	return creds.AuthToken, nil
}

// HasCredential reports whether a token can be read, without any network.
func HasCredential(login *multiacct.Login) bool {
	token, err := ReadToken(login)
	return err == nil && token != ""
}

// ReadCredentials parses a sign-in file.
func ReadCredentials(path string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, ErrNoCredentials
		}
		return Credentials{}, err
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return Credentials{}, fmt.Errorf("Freebuff credentials at %s are not valid JSON: %w", path, err)
	}
	creds.AuthToken = strings.TrimSpace(creds.AuthToken)
	if creds.AuthToken == "" {
		return Credentials{}, fmt.Errorf("Freebuff credentials at %s have no authToken. Sign in again.", path)
	}
	return creds, nil
}

// WriteCredentials stores a sign-in file owner-only, atomically.
func WriteCredentials(path string, creds Credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Client signs in. Only the device-code flow needs it; chat goes through the
// shared upstream client.
type Client struct {
	HTTP *http.Client
	// OpenBrowser opens the sign-in URL; nil uses the platform launcher.
	OpenBrowser func(url string)
	// PollEvery overrides the 5 second poll interval (tests).
	PollEvery time.Duration
	// Now / Sleep are injectable for tests.
	Sleep func(time.Duration)
}

func (c *Client) http() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) pollEvery() time.Duration {
	if c != nil && c.PollEvery > 0 {
		return c.PollEvery
	}
	return 5 * time.Second
}

func (c *Client) sleep(d time.Duration) {
	if c != nil && c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

// browserUA is sent on login calls only; the auth endpoints sit behind a
// browser-facing gate.
const browserUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/151.0.0.0 Safari/537.36"

func (c *Client) json(ctx context.Context, method, target string, body any, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if out != nil && len(data) > 0 {
		_ = json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}

func fingerprintID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

// SignIn runs the device-code flow: print/open the login URL, poll until the
// user finishes in the browser, then store the token at SessionPath(login).
func (c *Client) SignIn(ctx context.Context, endpoint string, login *multiacct.Login) (Credentials, error) {
	endpoint = EndpointFromBaseURL(endpoint)
	fid := fingerprintID()

	var code struct {
		LoginURL        string `json:"loginUrl"`
		FingerprintHash string `json:"fingerprintHash"`
		// expiresAt is a number (epoch ms) on the real server; keep the raw
		// token so it is echoed back to the poll byte for byte.
		ExpiresAt   json.Number `json:"expiresAt"`
		ExpiresInMs int64       `json:"expiresInMs"`
	}
	status, err := c.json(ctx, http.MethodPost, endpoint+"/api/auth/cli/code", map[string]string{"fingerprintId": fid}, &code)
	if err != nil {
		return Credentials{}, err
	}
	if status != http.StatusOK || code.LoginURL == "" {
		return Credentials{}, fmt.Errorf("Freebuff sign-in could not start (HTTP %d)", status)
	}
	if c != nil && c.OpenBrowser != nil {
		c.OpenBrowser(code.LoginURL)
	} else {
		openBrowser(code.LoginURL)
	}
	fmt.Fprintf(os.Stderr, "Open this URL to sign in to Freebuff:\n  %s\n", code.LoginURL)

	deadline := time.Now().Add(10 * time.Minute)
	if code.ExpiresInMs > 0 {
		if d := time.Now().Add(time.Duration(code.ExpiresInMs) * time.Millisecond); d.Before(deadline) {
			deadline = d
		}
	}
	query := url.Values{
		"fingerprintId":   {fid},
		"fingerprintHash": {code.FingerprintHash},
		"expiresAt":       {code.ExpiresAt.String()},
	}
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return Credentials{}, err
		}
		var poll struct {
			Default *pollUser `json:"default"`
			User    *pollUser `json:"user"`
		}
		status, err := c.json(ctx, http.MethodGet, endpoint+"/api/auth/cli/status?"+query.Encode(), nil, &poll)
		if err == nil && status == http.StatusOK {
			u := poll.Default
			if u == nil {
				u = poll.User
			}
			if u != nil && u.AuthToken != "" {
				creds := Credentials{AuthToken: u.AuthToken, UserID: u.ID, Email: u.Email, Name: u.Name}
				if err := WriteCredentials(SessionPath(login), creds); err != nil {
					return Credentials{}, err
				}
				return creds, nil
			}
		}
		// 401 just means "not signed in yet"; anything else is retried too.
		c.sleep(c.pollEvery())
	}
	return Credentials{}, errors.New("Freebuff sign-in timed out; run it again and open the new URL")
}

type pollUser struct {
	AuthToken string `json:"authToken"`
	ID        string `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
}
