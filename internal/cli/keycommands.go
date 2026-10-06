package cli

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/xinyao27/jevonian/internal/keys"
	"github.com/xinyao27/jevonian/internal/paths"
)

func parseLimit(a arguments) (*float64, error) {
	raw, ok := a.flags["limit-usd"]
	if !ok {
		raw, ok = a.flags["limit"]
	}
	if !ok {
		return nil, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return nil, fmt.Errorf("--limit-usd must be a finite non-negative number (0 removes the limit)")
	}
	return &v, nil
}
func (c commandContext) keys(a arguments) error {
	command := "list"
	rest := a.positionals
	if len(rest) > 0 {
		command = rest[0]
		rest = rest[1:]
	}
	db, err := openLedger()
	if err != nil {
		return err
	}
	defer db.Close()
	store := keys.Open(paths.DataDir(), db)
	defer store.Close()
	switch command {
	case "list":
		list, err := store.ListWithUsage()
		if err != nil {
			return err
		}
		if a.has("json") {
			return json.NewEncoder(c.out).Encode(list)
		}
		if len(list) == 0 {
			fmt.Fprintln(c.out, "No keys. Create one with: jevonian keys create NAME")
			return nil
		}
		for _, k := range list {
			limit := "unlimited"
			if k.LimitUSD != nil {
				limit = fmt.Sprintf("$%.4f", *k.LimitUSD)
			}
			fmt.Fprintf(c.out, "%s  %-20s %s… requests=%d limit=%s\n", k.ID, k.Name, k.Prefix, k.Requests, limit)
		}
		return nil
	case "create", "add":
		limit, err := parseLimit(a)
		if err != nil {
			return err
		}
		name := a.flags["name"]
		if name == "" && len(rest) > 0 {
			name = rest[0]
		}
		created, err := store.Create(keys.CreateOptions{Name: name, LimitUSD: limit})
		if err != nil {
			return err
		}
		if a.has("json") {
			return json.NewEncoder(c.out).Encode(map[string]any{"key": created.Key, "record": created.Record})
		}
		fmt.Fprintf(c.out, "Created %s (%s)\n%s\nSave this key now; it is shown only once.\n", created.Record.ID, created.Record.Name, created.Key)
		return nil
	case "remove", "delete", "revoke", "update", "rename":
		if len(rest) != 1 {
			return fmt.Errorf("Usage: jevonian keys %s <id> [--name NAME] [--limit-usd N]", command)
		}
		limit, err := parseLimit(a)
		if err != nil {
			return err
		}
		name, nameSet := a.flags["name"]
		if (command == "update" || command == "rename") && !nameSet && limit == nil {
			return fmt.Errorf("keys %s requires --name and/or --limit-usd", command)
		}
		if nameSet && strings.TrimSpace(name) == "" {
			return fmt.Errorf("key name cannot be empty")
		}
		var found bool
		if command == "remove" || command == "delete" || command == "revoke" {
			found, err = store.Revoke(rest[0])
		} else {
			patch := keys.UpdateOptions{LimitUSD: limit, HasLimit: limit != nil}
			if nameSet {
				trimmed := strings.TrimSpace(name)
				patch.Name = &trimmed
			}
			_, found, err = store.Update(rest[0], patch)
		}
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("key %q not found", rest[0])
		}
		fmt.Fprintf(c.out, "Key %s %s\n", rest[0], command)
		return nil
	default:
		return fmt.Errorf("Usage: jevonian keys [list|create|update|remove]")
	}
}
