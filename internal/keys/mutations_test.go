package keys_test

import (
	"sync"
	"testing"

	"github.com/xinyao27/jevonian/internal/keys"
)

func TestKeyMutationPreservesConcurrentUsageAndRevocation(t *testing.T) {
	store := keys.Open(t.TempDir(), nil)
	defer store.Close()
	created, err := store.Create(keys.CreateOptions{Name: "before"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			decision, err := store.AllowKey(created.Key)
			if err != nil || !decision.Allowed {
				t.Errorf("key denied: %v %+v", err, decision)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			name := "renamed"
			if _, ok, err := store.Update(created.Record.ID, keys.UpdateOptions{Name: &name}); err != nil || !ok {
				t.Errorf("update: %v %v", ok, err)
			}
			if err := store.Flush(); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Requests != 100 || list[0].Name != "renamed" {
		t.Fatalf("lost usage or rename: %+v", list)
	}
	if ok, err := store.Revoke(created.Record.ID); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	if store.HasKeys() {
		t.Fatal("flush resurrected revoked key")
	}
}
