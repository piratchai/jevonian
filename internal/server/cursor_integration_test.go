package server_test

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/provider/cursor"
	"github.com/xinyao27/jevonian/internal/server"
)

func TestCursorIncrementalInferenceAndLeadingRefusal(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		t.Run(fmt.Sprintf("refusal=%v", refusal), func(t *testing.T) {
			release := make(chan struct{})
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/GetServerConfig") {
					fmt.Fprintf(w, `{"agentUrlConfig":{"agentUrl":"http://%s"}}`, r.Host)
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/Run") {
					t.Errorf("wrong Cursor endpoint %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if r.Header.Get("Authorization") != "Bearer cursor-token" {
					t.Errorf("wrong auth %v", r.Header)
				}
				if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
					t.Error(err)
					return
				}
				requestDone := make(chan struct{})
				go func() { io.Copy(io.Discard, r.Body); close(requestDone) }()
				w.Header().Set("Content-Type", "application/connect+proto")
				if refusal {
					w.Write(cursor.EncodeFrame([]byte(`{"error":{"code":"resource_exhausted","message":"quota exhausted"}}`), 2))
				} else {
					text := new(cursor.Pb).Bytes(1, new(cursor.Pb).Str(1, "incremental-cursor").Build()).Build()
					interaction := new(cursor.Pb).Bytes(1, text).Build()
					w.Write(cursor.EncodeFrame(interaction, 0))
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-release:
				}
				select {
				case <-requestDone:
				case <-release:
				}
			}))
			defer api.Close()
			defer close(release)
			healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"healthy-failover\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer healthy.Close()
			cfg := config.Config{DefaultProvider: "cursor", Providers: []config.Provider{
				{Name: "cursor", Type: config.ProviderTypeCursor, Auth: config.AuthOAuth, OAuthSource: config.OAuthStatic, APIKey: "cursor-token", BaseURL: api.URL, Models: []config.ModelEntry{{ID: "m"}}},
				{Name: "healthy", Type: config.ProviderTypeOpenAI, BaseURL: healthy.URL, APIKey: "k", Models: []config.ModelEntry{{ID: "m"}}},
			}}
			srv := httptest.NewServer(server.New("", server.Deps{Config: &cfg}).Handler())
			defer srv.Close()
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			expected := "incremental-cursor"
			provider := "cursor"
			if refusal {
				expected = "healthy-failover"
				provider = "healthy"
			}
			if resp.StatusCode != 200 || resp.Header.Get("X-Jevonian-Provider") != provider {
				t.Fatalf("wrong served provider: %d %v", resp.StatusCode, resp.Header)
			}
			scanner := bufio.NewScanner(resp.Body)
			seen := false
			for scanner.Scan() {
				if strings.Contains(scanner.Text(), expected) {
					seen = true
					break
				}
			}
			if !seen {
				t.Fatalf("no incremental output before upstream close: %v", scanner.Err())
			}
			resp.Body.Close()
		})
	}
}
