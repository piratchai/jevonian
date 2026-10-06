// Package admin serves the dashboard's /api/* endpoints (port of src/admin.ts,
// src/activity.ts, src/stats.ts and the admin halves of src/updates.ts,
// src/leaderboard.ts and src/trace.ts). JSON shapes match the TypeScript
// server field-for-field because web/src/lib/api.ts consumes them unchanged.
//
// # Mounting
//
// The handler routes paths *without* the /api prefix, exactly like the Hono
// sub-app it replaces, so the integration owner mounts it with StripPrefix on
// the loopback listener only (never on the tunnel/LAN surface):
//
//	adminHandler := admin.New(admin.Deps{
//		Config:  srv.Config,             // atomic snapshot getter
//		Reload:  srv.Reload,             // atomic swap after a config write
//		Ledger:  admin.OpenSQLiteLogs(paths.LedgerDBPath()),
//		Keys:    keyStore,
//		Quota:   tracker,
//		Guard:   g,
//		Routing: routingDeps,
//		Sessions: store,
//		Traces:  traces,
//		Tunnel:  tunnelManager,
//		Updates: updater,                // internal/cli.NewUpdater(...)
//		Lifecycle: drainer,              // admin.NewDrainer() (or server's own)
//		Restart: restartFn,
//		Brain:   brainClient,
//	})
//	mux.Handle("/api/", http.StripPrefix("/api", adminHandler))
//
// Every request is refused with 403 unless it comes from a loopback peer: the
// admin API has no authentication of its own, so it must never be reachable
// from the LAN or a tunnel even if mounted by mistake.
//
// # State
//
// The package owns no mutable package-level state. Config writes always build
// a fresh *config.Config (re-read from disk so external edits are merged, or a
// deep copy of the snapshot), persist it atomically (tmp + rename, 0600), then
// hand it to Deps.Reload. The live snapshot is never mutated in place.
package admin
