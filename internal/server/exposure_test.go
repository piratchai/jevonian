package server_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/keys"
	"github.com/xinyao27/jevonian/internal/server"
)

func TestPublicExposureKeysDynamicConfigAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := keys.Open(t.TempDir(), nil)
	defer store.Close()
	port := 0
	cfg := &config.Config{Lan: config.LanConfig{Enabled: true, Host: "127.0.0.1", Port: &port}, Routing: config.DefaultRouting()}
	var current atomic.Pointer[config.Config]
	current.Store(cfg)
	exposure := server.NewExposure(ctx, server.Deps{Keys: store}, current.Load, server.ExposureOptions{})
	defer exposure.Close()
	if err := exposure.Reconcile(cfg); err == nil {
		t.Fatal("opened LAN before any keys exist")
	}
	created, err := store.Create(keys.CreateOptions{Name: "LAN"})
	if err != nil {
		t.Fatal(err)
	}
	if err := exposure.Reconcile(cfg); err != nil {
		t.Fatal(err)
	}
	address, _ := exposure.Addresses()
	client := &http.Client{Timeout: time.Second}
	request := func(path, token string) int {
		req, _ := http.NewRequest("GET", "http://"+address+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := request("/v1/models", "jevonian-local"); status != 401 {
		t.Fatalf("LAN honors local sentinel: %d", status)
	}
	if status := request("/api/state", created.Key); status != 404 {
		t.Fatalf("admin exposed: %d", status)
	}
	if status := request("/providers", created.Key); status != 404 {
		t.Fatalf("dashboard exposed: %d", status)
	}
	if status := request("/v1/models", created.Key); status != 200 {
		t.Fatalf("real key rejected: %d", status)
	}
	next := *cfg
	next.Lan.Enabled = false
	current.Store(&next)
	if err := exposure.Reconcile(&next); err != nil {
		t.Fatal(err)
	}
	exposure.Close()
	conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("LAN listener still accepts after close")
	}
}

func TestPublicExposureBindFailureKeepsPreviousListener(t *testing.T) {
	store := keys.Open(t.TempDir(), nil)
	defer store.Close()
	if _, err := store.Create(keys.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	port := 0
	cfg := &config.Config{Lan: config.LanConfig{Enabled: true, Host: "127.0.0.1", Port: &port}}
	var current atomic.Pointer[config.Config]
	current.Store(cfg)
	calls := 0
	exposure := server.NewExposure(context.Background(), server.Deps{Keys: store}, current.Load, server.ExposureOptions{Listen: func(network, address string) (net.Listener, error) {
		calls++
		if calls > 1 {
			return nil, fmt.Errorf("occupied")
		}
		return net.Listen(network, address)
	}})
	defer exposure.Close()
	if err := exposure.Reconcile(cfg); err != nil {
		t.Fatal(err)
	}
	original, _ := exposure.Addresses()
	newPort := 9000
	next := *cfg
	next.Lan.Port = &newPort
	if err := exposure.Reconcile(&next); err == nil {
		t.Fatal("bind failure hidden")
	}
	preserved, _ := exposure.Addresses()
	if preserved != original {
		t.Fatalf("previous listener replaced: %s", preserved)
	}
}
