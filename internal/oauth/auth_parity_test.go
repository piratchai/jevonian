package oauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/provider/multiacct"
)

// testdata/auth_ts.json is src/auth.ts resolveProviderAuth over a matrix of
// provider type x host x key source x auth x custom headers x wire kind.
func TestResolveProviderAuthMatchesTS(t *testing.T) {
	raw, err := os.ReadFile("testdata/auth_ts.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Provider struct {
			Name        string            `json:"name"`
			Type        string            `json:"type"`
			BaseURL     string            `json:"baseUrl"`
			Auth        string            `json:"auth"`
			OAuthSource string            `json:"oauthSource"`
			APIKey      string            `json:"apiKey"`
			APIKeyEnv   string            `json:"apiKeyEnv"`
			NoKey       bool              `json:"noKey"`
			Headers     map[string]string `json:"headers"`
		}
		Kind    string
		Headers map[string]string
		Error   string
		Token   string
		Project string
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	creds := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(creds, []byte(`{"stored":{"apiKey":"sk-stored"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEVONIAN_CREDENTIALS", creds)
	t.Setenv("TEST_ENV_KEY", "sk-env")
	t.Setenv("JEVONIAN_ANTIGRAVITY_PROJECT", "proj-x")
	r := &Resolver{Credentials: &multiacct.Store{Path: creds}}
	bad := 0
	for i, row := range rows {
		p := config.Provider{
			Name: row.Provider.Name, Type: config.ProviderType(row.Provider.Type), BaseURL: row.Provider.BaseURL,
			Auth: config.ProviderAuth(row.Provider.Auth), OAuthSource: config.OAuthSource(row.Provider.OAuthSource),
			APIKey: row.Provider.APIKey, APIKeyEnv: row.Provider.APIKeyEnv, NoKey: row.Provider.NoKey,
			Headers: row.Provider.Headers, Billing: config.BillingAPI,
		}
		got, err := r.ResolveProviderAuth(t.Context(), p, WireKind(row.Kind), "sess1234")
		if (err != nil) != (row.Error != "") {
			bad++
			t.Errorf("#%d %+v kind=%s: err=%v want %q", i, row.Provider, row.Kind, err, row.Error)
		} else if err != nil {
			// Messages differ only in the Go-style casing check below.
			if err.Error() != row.Error {
				bad++
				t.Errorf("#%d error text: %q want %q", i, err.Error(), row.Error)
			}
		} else {
			want := row.Headers
			if want == nil {
				want = map[string]string{}
			}
			gh := got.Headers
			if gh == nil {
				gh = map[string]string{}
			}
			if !reflect.DeepEqual(gh, want) || got.Token != row.Token || got.Project != row.Project {
				bad++
				t.Errorf("#%d %+v kind=%s:\n got  %v tok=%q proj=%q\n want %v tok=%q proj=%q", i, row.Provider, row.Kind, gh, got.Token, got.Project, want, row.Token, row.Project)
			}
		}
		if bad > 8 {
			t.Fatal("too many diffs")
		}
	}
}
