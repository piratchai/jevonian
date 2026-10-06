package keys_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/keys"
)

// sk-jev-… is shown once; keys.json holds only its sha256, mode 0600 (src/keys.ts).
func TestKeysJSONStoresOnlyTheHash(t *testing.T) {
	dir := t.TempDir()
	store := keys.Open(dir, nil)
	limit := 2.5
	created, err := store.Create(keys.CreateOptions{Name: "  agent  ", LimitUSD: &limit})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Key, "sk-jev-") || len(created.Key) != len("sk-jev-")+48 {
		t.Fatalf("key shape %q", created.Key)
	}
	if created.Record.Prefix != created.Key[:11] || created.Record.Name != "agent" || created.Record.LimitUSD == nil || *created.Record.LimitUSD != 2.5 {
		t.Fatalf("record %+v", created.Record)
	}
	path := keys.Path(dir)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(created.Key))
	if strings.Contains(string(raw), created.Key) || !strings.Contains(string(raw), hex.EncodeToString(sum[:])) {
		t.Fatalf("keys.json must hold the sha256 only: %s", raw)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	// Rename, change and clear the credit limit; revoke.
	name := "renamed"
	updated, ok, err := store.Update(created.Record.ID, keys.UpdateOptions{Name: &name})
	if err != nil || !ok || updated.Name != "renamed" || updated.LimitUSD == nil {
		t.Fatalf("rename %+v %v %v", updated, ok, err)
	}
	updated, _, _ = store.Update(created.Record.ID, keys.UpdateOptions{HasLimit: true})
	if updated.LimitUSD != nil {
		t.Fatalf("limit not cleared: %+v", updated)
	}
	if removed, err := store.Revoke(created.Record.ID); err != nil || !removed {
		t.Fatalf("revoke %v %v", removed, err)
	}
	if removed, _ := store.Revoke(created.Record.ID); removed {
		t.Fatal("double revoke")
	}
}
