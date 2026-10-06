package cli

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/paths"
	"github.com/xinyao27/jevonian/internal/provider/freebuff"
	"github.com/xinyao27/jevonian/internal/provider/multiacct"
	"github.com/xinyao27/jevonian/internal/provider/workbuddy"
	"github.com/xinyao27/jevonian/internal/routing"
)

func (c commandContext) interactive(a arguments) bool {
	f, ok := c.in.(*os.File)
	return ok && !a.has("yes") && isatty.IsTerminal(f.Fd())
}
func (c commandContext) ask(r *bufio.Reader, label, def string) (string, error) {
	fmt.Fprintf(c.out, "%s", label)
	if def != "" {
		fmt.Fprintf(c.out, " [%s]", def)
	}
	fmt.Fprint(c.out, ": ")
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		line = def
	}
	return line, nil
}

// askSecret uses the system terminal utility, not a CGO terminal dependency.
// It restores echo even if the input read fails. Noninteractive inputs are never
// sent through stty (and use --key/--env rather than this prompt).
func (c commandContext) askSecret(reader *bufio.Reader) (string, error) {
	file, ok := c.in.(*os.File)
	if !ok {
		return "", fmt.Errorf("secret prompt requires a terminal; use --env or --key")
	}
	state := exec.Command("stty", "-g")
	state.Stdin = file
	raw, err := state.Output()
	if err != nil {
		return "", fmt.Errorf("cannot hide terminal input; use --env or --key: %w", err)
	}
	set := exec.Command("stty", "-echo")
	set.Stdin = file
	if err := set.Run(); err != nil {
		return "", fmt.Errorf("cannot hide terminal input; use --env or --key: %w", err)
	}
	defer func() {
		restore := exec.Command("stty", strings.TrimSpace(string(raw)))
		restore.Stdin = file
		_ = restore.Run()
		fmt.Fprintln(c.out)
	}()
	fmt.Fprint(c.out, "API key (enter to use an environment variable): ")
	line, err := reader.ReadString('\n')
	return strings.TrimSpace(line), err
}
func keySource(p config.Provider) string {
	if p.Auth == config.AuthOAuth && p.OAuthSource != "" && p.OAuthSource != config.OAuthStatic {
		r := oauth.Resolver{}
		if r.HasCredential(string(p.OAuthSource), p.Login) {
			return "oauth:" + string(p.OAuthSource)
		}
		return "none"
	}
	if p.APIKey != "" {
		return "inline"
	}
	if multiacct.DefaultStore().Get(p.Name) != "" {
		if p.Auth == config.AuthOAuth {
			return "oauth:static"
		}
		return "credentials"
	}
	if p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) != "" {
		return "env:" + p.APIKeyEnv
	}
	return "none"
}
func (c commandContext) providers() error {
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	if len(cfg.Providers) == 0 {
		fmt.Fprintln(c.out, "No providers configured. Run `jevonian add`.")
		return nil
	}
	for _, p := range cfg.Providers {
		source := keySource(p)
		if source == "none" && p.APIKeyEnv != "" {
			source = "missing (" + p.APIKeyEnv + ")"
		}
		typ := string(p.Type)
		if p.Billing == config.BillingSubscription {
			typ += "/sub"
		}
		account := ""
		if p.Login != nil {
			where := p.Login.Label
			if where == "" {
				where = p.Login.CredentialsPath
			}
			if where == "" {
				where = p.Login.Home
			}
			if where == "" {
				where = p.Login.KeychainService
			}
			if where == "" {
				where = "custom"
			}
			account = " account=" + c.styleValue(where)
		}
		fmt.Fprintf(c.out, "%s %-14s %-46s key=%s%s models=%d\n", c.styleValue(fmt.Sprintf("%-24s", p.Name)), typ, p.BaseURL, c.styleValue(source), account, len(p.Models))
	}
	return nil
}
func (c commandContext) remove(a arguments) error {
	if len(a.positionals) != 1 {
		return fmt.Errorf("Usage: jevonian remove <provider> [--keep-key]")
	}
	name := a.positionals[0]
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	found := false
	next := make([]config.Provider, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p.Name == name {
			found = true
		} else {
			next = append(next, p)
		}
	}
	if !found {
		return fmt.Errorf("Provider %q not found", name)
	}
	cfg.Providers = next
	if cfg.DefaultProvider == name {
		cfg.DefaultProvider = ""
		if len(next) > 0 {
			cfg.DefaultProvider = next[0].Name
		}
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}
	if !a.has("keep-key") {
		if err := multiacct.DefaultStore().Remove(name); err != nil {
			return err
		}
	}
	suffix := ""
	if a.has("keep-key") {
		suffix = " (key kept)"
	}
	fmt.Fprintf(c.out, "Removed provider %s%s\n", c.styleValue(fmt.Sprintf("%q", name)), suffix)
	return nil
}
func expandHome(p string) string {
	h, _ := os.UserHomeDir()
	if p == "~" {
		return h
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(h, p[2:])
	}
	return p
}
func (c commandContext) add(a arguments) error {
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	interactive := c.interactive(a)
	reader := bufio.NewReader(c.in)
	id := ""
	if len(a.positionals) > 0 {
		id = a.positionals[0]
	}
	if id == "" && interactive {
		var names []string
		for _, p := range presets {
			names = append(names, p.id)
		}
		fmt.Fprintln(c.out, "Presets: "+strings.Join(names, ", ")+", custom")
		id, err = c.ask(reader, "Provider", "deepseek")
		if err != nil {
			return err
		}
	}
	if id == "" {
		return fmt.Errorf("Usage: jevonian add <provider> [--key K] [--env NAME] [--models a,b] [--base-url URL] [--type TYPE]")
	}
	p := config.Provider{Name: id, Type: config.ProviderTypeOpenAI, Auth: config.AuthAPIKey, Billing: config.BillingAPI, InjectStreamUsage: true, Models: []config.ModelEntry{}}
	pre := findPreset(id)
	if pre != nil {
		p = pre.provider()
	} else if meta, ok := loadPricing().Providers[id]; ok {
		p.BaseURL = meta.API
		p.Type = config.ProviderType(meta.Type)
		if len(meta.Env) > 0 {
			p.APIKeyEnv = meta.Env[0]
		}
	}
	for flag, dest := range map[string]*string{"name": &p.Name, "base-url": &p.BaseURL, "env": &p.APIKeyEnv} {
		if v, ok := a.flags[flag]; ok {
			*dest = strings.TrimSpace(v)
		}
	}
	if typ, ok := a.flags["type"]; ok {
		p.Type = config.ProviderType(typ)
	}
	if auth, ok := a.flags["auth"]; ok {
		if auth != "oauth" && auth != "api-key" {
			return fmt.Errorf("--auth must be api-key or oauth")
		}
		p.Auth = config.ProviderAuth(auth)
	}
	if billing, ok := a.flags["billing"]; ok {
		if billing != "api" && billing != "subscription" {
			return fmt.Errorf("--billing must be api or subscription")
		}
		p.Billing = config.ProviderBilling(billing)
	}
	if source, ok := a.flags["oauth-source"]; ok {
		if !oauth.IsSource(source) {
			return fmt.Errorf("unknown --oauth-source %q; use %s", source, strings.Join(oauth.Sources, ", "))
		}
		p.OAuthSource = config.OAuthSource(source)
	}
	if p.Auth == config.AuthOAuth {
		if p.OAuthSource == "" {
			p.OAuthSource = config.OAuthStatic
		}
	} else {
		if a.has("oauth-source") {
			return fmt.Errorf("--oauth-source requires --auth oauth")
		}
		p.OAuthSource = ""
	}
	typ, err := oauth.WireType(string(p.OAuthSource), string(p.Type), a.has("type"))
	if err != nil {
		return err
	}
	p.Type = config.ProviderType(typ)
	if mismatch := oauth.WireMismatch(typ, string(p.Auth), string(p.OAuthSource)); mismatch != "" {
		return fmt.Errorf("%s", mismatch)
	}
	if p.BaseURL == "" && interactive {
		p.Name, err = c.ask(reader, "Provider name", p.Name)
		if err != nil {
			return err
		}
		p.BaseURL, err = c.ask(reader, "Base URL", "")
		if err != nil {
			return err
		}
		t, err := c.ask(reader, "Protocol type", string(p.Type))
		if err != nil {
			return err
		}
		p.Type = config.ProviderType(t)
	}
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	u, e := url.Parse(p.BaseURL)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("a valid http(s) --base-url is required for provider %q", id)
	}
	login := &config.ProviderLogin{Label: a.flags["login-label"], Home: expandHome(a.flags["login-home"]), CredentialsPath: expandHome(a.flags["login-file"])}
	login.KeychainService, login.KeychainAccount, _ = strings.Cut(a.flags["login-keychain"], ":")
	if login.Label != "" || login.Home != "" || login.CredentialsPath != "" || login.KeychainService != "" {
		if p.Auth != config.AuthOAuth {
			return fmt.Errorf("--login-home / --login-file / --login-keychain require --auth oauth")
		}
		p.Login = login
	}
	// Reject malformed protocol and login fields before discovery can read tokens.
	validationCfg := config.DefaultConfig()
	validationCfg.Providers = []config.Provider{p}
	validationDoc, err := configDocument(validationCfg)
	if err != nil {
		return err
	}
	if _, err := config.ParseConfig(validationDoc); err != nil {
		return err
	}
	if mismatch := oauth.WireMismatch(string(p.Type), string(p.Auth), string(p.OAuthSource)); mismatch != "" {
		return fmt.Errorf("%s", mismatch)
	}
	if pre != nil {
		if pre.source != "" && pre.hint != "" {
			fmt.Fprintln(c.out, pre.hint)
		} else if pre.keysURL != "" {
			fmt.Fprintf(c.out, "Get a key at %s\n", c.styleValue(pre.keysURL))
		} else if pre.hint != "" {
			fmt.Fprintln(c.out, pre.hint)
		}
	}
	key := a.flags["key"]
	live := p.Auth == config.AuthOAuth && p.OAuthSource != config.OAuthStatic
	if live && key != "" {
		return fmt.Errorf("--key is not used for live OAuth; use --oauth-source static to store a token")
	}
	if !live && !p.NoKey && key == "" && !a.has("env") && interactive {
		key, err = c.askSecret(reader)
		if err != nil {
			return err
		}
	}
	if p.OAuthSource == config.OAuthFreebuff && !freebuff.HasCredential(p.Login) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		signed, err := (&freebuff.Client{HTTP: cliHTTP()}).SignIn(ctx, freebuff.EndpointFromBaseURL(p.BaseURL), p.Login)
		if err != nil {
			return err
		}
		who := signed.Email
		if who == "" {
			who = signed.Name
		}
		fmt.Fprintf(c.out, "Signed in to Freebuff as %s.\n", c.styleValue(who))
	}
	if p.OAuthSource == config.OAuthWorkbuddyAI && !workbuddy.HasCredential(p.Login) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		signed, err := (&workbuddy.Client{HTTP: cliHTTP()}).SignIn(ctx, workbuddy.EndpointFromBaseURL(p.BaseURL), p.Login)
		if err != nil {
			return err
		}
		fmt.Fprintf(c.out, "Signed in as %s.\n", c.styleValue(signed.User))
	}
	models := splitModels(a.flags["models"])
	if len(models) == 0 {
		probe := p
		probe.APIKey = key
		entry := discoverProvider(probe)
		if entry.Error != "" {
			fmt.Fprintf(c.errOut, "model discovery failed: %s\n", entry.Error)
		} else {
			models = entry.Models
		}
	}
	if len(models) == 0 && !live && !p.NoKey {
		snapshot := loadPricing()
		prefix := id + "/"
		for model := range snapshot.Models {
			if strings.HasPrefix(model, prefix) {
				models = append(models, strings.TrimPrefix(model, prefix))
			}
		}
		models = sortedUnique(models)
	}
	if len(models) == 0 && interactive {
		typed, err := c.ask(reader, "Model ids (comma separated; empty to skip)", "")
		if err != nil {
			return err
		}
		models = splitModels(typed)
	}
	for _, id := range models {
		p.Models = append(p.Models, config.ModelEntry{ID: id})
	}
	index := -1
	for i, previous := range cfg.Providers {
		if previous.Name != p.Name {
			continue
		}
		index = i
		p.ExcludeModels = reconcileExcluded(previous, models)
		if previous.SyncModels != nil {
			p.SyncModels = previous.SyncModels
		}
		break
	}
	// Validate before touching the credential store or disk.
	probe := cfg
	if index >= 0 {
		probe.Providers = append([]config.Provider{}, cfg.Providers...)
		probe.Providers[index] = p
	} else {
		probe.Providers = append(append([]config.Provider{}, cfg.Providers...), p)
	}
	doc, err := configDocument(probe)
	if err != nil {
		return err
	}
	validated, err := config.ParseConfig(doc)
	if err != nil {
		return err
	}
	cfg = validated
	if cfg.DefaultProvider == "" {
		cfg.DefaultProvider = p.Name
	}
	derived := routing.DeriveRoutings(&cfg, pricingDeps())
	for i := range cfg.Routing.Routings {
		if len(cfg.Routing.Routings[i].Models) > 0 {
			continue
		}
		for _, r := range derived {
			if r.ID == cfg.Routing.Routings[i].ID {
				cfg.Routing.Routings[i].Models = append([]string{}, r.Models...)
			}
		}
	}
	cfg.Routing.Tiers = configuredTiers(cfg.Routing.Routings)
	if cfg.Routing.BaselineModel == "" {
		for _, r := range cfg.Routing.Routings {
			if r.ID == "plan" && len(r.Models) > 0 {
				cfg.Routing.BaselineModel = r.Models[0]
			}
		}
	}
	if key != "" {
		if err := multiacct.DefaultStore().Set(p.Name, key); err != nil {
			return err
		}
		for i := range cfg.Providers {
			if cfg.Providers[i].Name == p.Name {
				cfg.Providers[i].APIKeyEnv = ""
			}
		}
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}
	if len(models) > 0 {
		preview := ""
		if len(models) <= 8 {
			preview = ": " + c.styleValue(strings.Join(models, ", "))
		}
		fmt.Fprintf(c.out, "enabling %s models%s\n", c.styleValue(fmt.Sprint(len(models))), preview)
	}
	authLine := "none"
	switch {
	case p.Auth == config.AuthOAuth:
		source := string(p.OAuthSource)
		if source == "" {
			source = "static"
		}
		authLine = "oauth (" + source + ")"
	case key != "":
		authLine = fmt.Sprintf("stored in %s (0600)", credentialsPath())
	case p.APIKeyEnv != "":
		authLine = "env " + p.APIKeyEnv
	}
	fmt.Fprintf(c.out, "\nAdded provider %s (%s) with %s models\n  %s %s\n", c.styleValue(fmt.Sprintf("%q", p.Name)), p.Type, c.styleValue(fmt.Sprint(len(models))), c.styleValue("auth:"), authLine)
	if p.Login != nil {
		where := p.Login.CredentialsPath
		if where == "" {
			where = p.Login.Home
		}
		if where == "" && p.Login.KeychainService != "" {
			where = "keychain:" + p.Login.KeychainService
		}
		if where == "" {
			where = "agent default"
		}
		label := ""
		if p.Login.Label != "" {
			label = p.Login.Label + " — "
		}
		fmt.Fprintf(c.out, "  %s %s%s\n", c.styleValue("login:"), label, where)
	}
	fmt.Fprintf(c.out, "  %s %s\n", c.styleValue("billing:"), p.Billing)
	for _, r := range cfg.Routing.Routings {
		if len(r.Models) > 0 {
			fmt.Fprintf(c.out, "  %s: %s\n", c.styleValue(r.ID), strings.Join(r.Models, ", "))
		}
	}
	fmt.Fprintf(c.out, "config: %s\nNext: jevonian serve\n", paths.ConfigPath())
	return nil
}
func reconcileExcluded(p config.Provider, models []string) []string {
	enabled := map[string]bool{}
	for _, id := range models {
		enabled[id] = true
	}
	out := append([]string{}, p.ExcludeModels...)
	for _, m := range p.Models {
		if !enabled[m.ID] {
			out = append(out, m.ID)
		}
	}
	var keep []string
	for _, id := range out {
		if !enabled[id] {
			keep = append(keep, id)
		}
	}
	return sortedUnique(keep)
}
func splitModels(value string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range strings.Split(value, ",") {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
}
func (c commandContext) init(a arguments) error {
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	if c.interactive(a) {
		if len(cfg.Providers) > 0 {
			fmt.Fprintf(c.out, "Config already exists at %s with %d providers.\nAdd another with: jevonian add\n", c.styleValue(paths.ConfigPath()), len(cfg.Providers))
			return nil
		}
		fmt.Fprintln(c.out, "Welcome to Jevonian. Let's add your first provider.")
		return c.add(a)
	}
	p := findPreset("deepseek").provider()
	p.Models = []config.ModelEntry{{ID: "deepseek-v4.1-flash"}, {ID: "deepseek-v4-pro"}}
	cfg = config.DefaultConfig()
	cfg.DefaultProvider = p.Name
	cfg.Providers = []config.Provider{p}
	for i := range cfg.Routing.Routings {
		m := "deepseek-v4.1-flash"
		if cfg.Routing.Routings[i].ID == "plan" {
			m = "deepseek-v4-pro"
		}
		cfg.Routing.Routings[i].Models = []string{m}
	}
	cfg.Routing.Tiers = routing.DeriveTiers(&cfg, pricingDeps())
	cfg.Routing.BaselineModel = "deepseek-v4-pro"
	if err := saveConfig(cfg); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Wrote %s\nSet DEEPSEEK_API_KEY, then run: jevonian serve\n", c.styleValue(paths.ConfigPath()))
	return nil
}

// credentialsPath mirrors multiacct's default resolution (JEVONIAN_CREDENTIALS, else XDG config).
func credentialsPath() string {
	if v := os.Getenv("JEVONIAN_CREDENTIALS"); v != "" {
		return v
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "jevonian", "credentials.json")
}
