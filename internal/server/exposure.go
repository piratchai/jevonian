package server

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/tunnel"
)

// Exposure owns the authenticated LAN and tunnel-origin listeners. Both reuse
// the main server's immutable config and process resources, but never its admin
// handler. No per-request code takes the lifecycle lock.
type Exposure struct {
	mu      sync.Mutex
	ctx     context.Context
	deps    Deps
	current func() *config.Config
	listen  func(string, string) (net.Listener, error)
	log     func(error)
	lan     *exposedListener
	origin  *exposedListener
	closed  bool
	wg      sync.WaitGroup
}

type exposedListener struct {
	address  string
	cancel   context.CancelFunc
	done     chan struct{}
	listener net.Listener
}

type ExposureOptions struct {
	Listen  func(string, string) (net.Listener, error)
	OnError func(error)
}

func NewExposure(ctx context.Context, deps Deps, current func() *config.Config, opts ExposureOptions) *Exposure {
	if opts.Listen == nil {
		opts.Listen = net.Listen
	}
	deps.Public = true
	deps.Admin = nil
	deps.ConfigSource = current
	deps.OnReload = nil
	return &Exposure{ctx: ctx, deps: deps, current: current, listen: opts.Listen, log: opts.OnError}
}

// Reconcile binds new addresses before replacing old listeners. Failure leaves
// the prior listener in place and is reported; it never starts an unauthenticated
// public surface when no Jevonian keys exist.
func (e *Exposure) Reconcile(cfg *config.Config) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("public listeners are closed")
	}
	if cfg == nil {
		return fmt.Errorf("public listener config is unavailable")
	}
	authorized := e.deps.Keys != nil && e.deps.Keys.HasKeys()
	lanAddress := ""
	if cfg.Lan.Enabled {
		if !authorized {
			return fmt.Errorf("create a Jevonian API key before enabling LAN")
		}
		lanAddress = net.JoinHostPort(tunnel.LanBindHost(cfg.Lan), fmt.Sprint(tunnel.LanPort(cfg)))
	}
	originAddress := ""
	if cfg.Tunnel.Enabled {
		if !authorized {
			return fmt.Errorf("create a Jevonian API key before enabling a tunnel")
		}
		port := cfg.Listen.Port + 1
		if cfg.Tunnel.PublicPort != nil {
			port = *cfg.Tunnel.PublicPort
		}
		originAddress = net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	}
	if err := e.replace(&e.lan, lanAddress); err != nil {
		return fmt.Errorf("LAN listener: %w", err)
	}
	if err := e.replace(&e.origin, originAddress); err != nil {
		return fmt.Errorf("tunnel listener: %w", err)
	}
	return nil
}

func (e *Exposure) replace(slot **exposedListener, address string) error {
	previous := *slot
	if previous != nil && previous.address == address {
		return nil
	}
	var next *exposedListener
	if address != "" {
		ln, err := e.listen("tcp", address)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(e.ctx)
		next = &exposedListener{address: address, cancel: cancel, done: make(chan struct{}), listener: ln}
		server := NewPublicServer(address, e.deps)
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			defer close(next.done)
			if err := server.Serve(ctx, ln); err != nil && e.log != nil {
				e.log(err)
			}
		}()
	}
	*slot = next
	if previous != nil {
		previous.cancel()
	}
	return nil
}

// Close stops accepting connections and drains both listeners before resource
// owners close the shared ledger, key store or transport.
func (e *Exposure) Close() {
	e.mu.Lock()
	e.closed = true
	if e.lan != nil {
		e.lan.cancel()
	}
	if e.origin != nil {
		e.origin.cancel()
	}
	e.mu.Unlock()
	e.wg.Wait()
}

func (e *Exposure) Addresses() (lan, origin string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lan != nil {
		lan = e.lan.listener.Addr().String()
	}
	if e.origin != nil {
		origin = e.origin.listener.Addr().String()
	}
	return
}
