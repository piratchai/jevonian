import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { type AppState } from "./admin";
import { parseConfig } from "./config";
import { createKey } from "./keys";
import { SessionStore } from "./routing";
import { createPublicApp } from "./server";

/**
 * The LAN surface binds `createPublicApp` — the same `/v1`-only app the tunnel forwards to.
 * These tests pin the two properties that make exposing it safe: the dashboard/admin API is
 * not served on that listener, and every request must carry a real Jevonian key.
 */
let home = "";

beforeEach(() => {
  home = mkdtempSync(join(tmpdir(), "jev-lan-surface-"));
  vi.stubEnv("JEVONIAN_DATA_DIR", home);
});

afterEach(() => {
  vi.unstubAllEnvs();
  rmSync(home, { recursive: true, force: true });
});

function app() {
  const config = parseConfig({
    listen: { host: "127.0.0.1", port: 8787 },
    lan: { enabled: true },
    providers: [],
  });
  const state: AppState = { config };
  return createPublicApp(state, new SessionStore(60_000));
}

describe("LAN surface", () => {
  it("serves /v1 and never the admin API", async () => {
    const { key } = createKey("lan-owner");
    const server = app();
    const auth = { authorization: `Bearer ${key}` };
    expect((await server.request("/v1/models", { headers: auth })).status).toBe(200);
    // The dashboard and admin API stay on loopback: exposing them on a LAN IP would let a
    // neighbour edit providers and read keys. Even an authenticated caller gets no route.
    expect((await server.request("/api/state", { headers: auth })).status).toBe(404);
    expect((await server.request("/api/keys", { headers: auth })).status).toBe(404);
    expect((await server.request("/", { headers: auth })).status).toBe(404);
  });

  it("refuses /v1 without a key even when a key exists", async () => {
    createKey("lan-owner");
    const response = await app().request("/v1/models");
    expect(response.status).toBe(401);
  });

  it("accepts a real Jevonian key", async () => {
    const { key } = createKey("lan-owner");
    const response = await app().request("/v1/models", {
      headers: { authorization: `Bearer ${key}` },
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as { object?: string; data?: unknown[] };
    expect(body.object).toBe("list");
    expect(Array.isArray(body.data)).toBe(true);
  });

  it("rejects the loopback desktop sentinel, which only the local app accepts", async () => {
    createKey("lan-owner");
    const response = await app().request("/v1/models", {
      headers: { authorization: "Bearer jevonian-local" },
    });
    expect(response.status).toBe(401);
  });

  it("says how to make a key when none exists", async () => {
    const response = await app().request("/v1/models");
    expect(response.status).toBe(401);
    const body = (await response.json()) as { error?: { message?: string } };
    expect(body.error?.message).toContain("No Jevonian API key exists yet");
  });
});
