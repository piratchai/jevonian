package chatgptweb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/wire"
)

// DefaultBaseURL points to the default local Chrome CDP debugging endpoint.
const DefaultBaseURL = "http://127.0.0.1:9222"

// Model represents an available ChatGPT model exposed in the user's account.
type Model struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ChatRequest represents an incoming completion request.
type ChatRequest struct {
	Body                wire.Body
	Model               string
	Stream              bool
	KeepWindowOnFailure bool
}

// ChatResult represents the outcome of a chat request.
type ChatResult struct {
	Completion wire.Body
	Stream     io.ReadCloser
	Status     int
	Error      *Error
}

// Kind classifies the root cause of an error.
type Kind string

const (
	KindAuth      Kind = "auth"
	KindModel     Kind = "model"
	KindRateLimit Kind = "rate_limit"
	KindInvalid   Kind = "invalid"
	KindBrowser   Kind = "browser"
	KindTimeout   Kind = "timeout"
)

// Error represents a structured error returned by the ChatGPT Web provider.
type Error struct {
	Status  int    `json:"status"`
	Kind    Kind   `json:"kind"`
	Message string `json:"message"`
}

// Error implements the standard Go error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("[%s] %s", e.Kind, e.Message)
}

// BrowserDriver abstracts browser automation for testing.
type BrowserDriver interface {
	Models(ctx context.Context, cdpEndpoint string) ([]Model, error)
	Chat(ctx context.Context, cdpEndpoint string, req ChatRequest, prompt string) (ChatResult, error)
}

// Provider implements browser automation against an existing logged-in Chrome instance.
type Provider struct {
	BaseURL    string
	HTTPClient *http.Client
	config     config.Provider
	driver     BrowserDriver
}

var defaultDriverForTest BrowserDriver
var defaultDriverMu sync.RWMutex

// SetDefaultDriverForTest overrides the browser driver for testing across instances.
func SetDefaultDriverForTest(d BrowserDriver) {
	defaultDriverMu.Lock()
	defer defaultDriverMu.Unlock()
	defaultDriverForTest = d
}

// NewProvider creates a new ChatGPT Web provider instance.
func NewProvider(p config.Provider, httpClient *http.Client) *Provider {
	baseURL := p.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	defaultDriverMu.RLock()
	driver := defaultDriverForTest
	defaultDriverMu.RUnlock()
	return &Provider{
		BaseURL:    baseURL,
		HTTPClient: httpClient,
		config:     p,
		driver:     driver,
	}
}

// SetDriver sets a custom BrowserDriver (used for testing or mock environments).
func (p *Provider) SetDriver(d BrowserDriver) {
	p.driver = d
}
