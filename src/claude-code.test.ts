import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import {
  applyClaudeCode,
  claudeCodeEnv,
  claudeCodeModelLabel,
  claudeCodeSettingsPath,
  resolveClaudeCodeModels,
  restoreClaudeCode,
} from "./claude-code";
import { jsoncObjectKeys } from "./jsonc";

describe("Claude Code model routing", () => {
  it("labels jevonian ids the way Desktop stand-ins do", () => {
    expect(claudeCodeModelLabel("jevonian/auto")).toBe("Jevonian Auto");
    expect(claudeCodeModelLabel("jevonian/utility")).toBe("Jevonian Utility");
  });

  it("uses utility for haiku when offered, otherwise the primary", () => {
    expect(resolveClaudeCodeModels(["jevonian/auto", "jevonian/utility"])).toEqual({
      primary: "jevonian/auto",
      fast: "jevonian/utility",
    });
    expect(resolveClaudeCodeModels(["jevonian/auto"])).toEqual({
      primary: "jevonian/auto",
      fast: "jevonian/auto",
    });
    expect(resolveClaudeCodeModels(["jevonian/auto"], "jevonian/plan")).toEqual({
      primary: "jevonian/plan",
      fast: "jevonian/plan",
    });
  });

  it("builds Ollama-style env with Jevonian picker labels", () => {
    const env = claudeCodeEnv({
      port: 8787,
      models: ["jevonian/auto", "jevonian/utility"],
    });
    expect(env.ANTHROPIC_BASE_URL).toBe("http://127.0.0.1:8787");
    expect(env.ANTHROPIC_AUTH_TOKEN).toBe("jevonian-local");
    expect(env.ANTHROPIC_API_KEY).toBe("");
    expect(env.ANTHROPIC_DEFAULT_OPUS_MODEL).toBe("jevonian/auto");
    expect(env.ANTHROPIC_DEFAULT_SONNET_MODEL).toBe("jevonian/auto");
    expect(env.ANTHROPIC_DEFAULT_HAIKU_MODEL).toBe("jevonian/utility");
    expect(env.CLAUDE_CODE_SUBAGENT_MODEL).toBe("jevonian/auto");
    expect(env.ANTHROPIC_DEFAULT_SONNET_MODEL_NAME).toBe("Jevonian Auto");
    expect(env.ANTHROPIC_CUSTOM_MODEL_OPTION).toBe("jevonian/auto");
    expect(env.ANTHROPIC_CUSTOM_MODEL_OPTION_NAME).toBe("Jevonian Auto");
  });
});

describe("Claude Code settings connect", () => {
  let home: string;

  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), "jev-claude-code-"));
    mkdirSync(join(home, ".claude"), { recursive: true });
    vi.stubEnv("HOME", home);
    vi.stubEnv("USERPROFILE", home);
    vi.stubEnv("JEVONIAN_DATA_DIR", join(home, "data"));
    // Ignore a real CLAUDE_CONFIG_DIR from the developer machine.
    vi.stubEnv("CLAUDE_CONFIG_DIR", join(home, ".claude"));
  });

  afterEach(() => {
    vi.unstubAllEnvs();
    rmSync(home, { recursive: true, force: true });
  });

  it("writes managed env into settings.json and restores prior content", () => {
    const path = claudeCodeSettingsPath();
    writeFileSync(
      path,
      `${JSON.stringify({ env: { API_TIMEOUT_MS: "1200000", KEEP_ME: "yes" }, theme: "dark" }, null, 2)}\n`,
    );

    const applied = applyClaudeCode({ port: 8787, models: ["jevonian/auto", "jevonian/utility"] });
    expect(applied.status.status).toBe("connected");
    expect(applied.status.baseUrl).toBe("http://127.0.0.1:8787");

    const connected = JSON.parse(readFileSync(path, "utf8")) as {
      theme: string;
      env: Record<string, string>;
    };
    expect(connected.theme).toBe("dark");
    expect(connected.env.KEEP_ME).toBe("yes");
    expect(connected.env.API_TIMEOUT_MS).toBe("1200000");
    expect(connected.env.ANTHROPIC_BASE_URL).toBe("http://127.0.0.1:8787");
    expect(connected.env.ANTHROPIC_DEFAULT_HAIKU_MODEL).toBe("jevonian/utility");
    expect(connected.env.ANTHROPIC_CUSTOM_MODEL_OPTION_NAME).toBe("Jevonian Auto");

    restoreClaudeCode();
    const restored = JSON.parse(readFileSync(path, "utf8")) as {
      theme: string;
      env: Record<string, string>;
    };
    expect(restored).toEqual({
      env: { API_TIMEOUT_MS: "1200000", KEEP_ME: "yes" },
      theme: "dark",
    });
  });

  it("creates settings.json when absent and removes it on restore", () => {
    applyClaudeCode({ port: 9090, models: ["jevonian/auto"] });
    const path = claudeCodeSettingsPath();
    const written = JSON.parse(readFileSync(path, "utf8")) as { env: Record<string, string> };
    expect(written.env.ANTHROPIC_BASE_URL).toBe("http://127.0.0.1:9090");

    restoreClaudeCode();
    expect(existsSync(path)).toBe(false);
  });

  it("edits surgically: comments, key order, and formatting survive connect", () => {
    const path = claudeCodeSettingsPath();
    const original = `{
  // my hand-written layout
  "theme": "dark",
  "env": {
    "API_TIMEOUT_MS": "1200000",   /* keep */
    "KEEP_ME": "yes"
  },
  "attribution": { "commit": "" }
}
`;
    writeFileSync(path, original);

    applyClaudeCode({ port: 8787, models: ["jevonian/auto"] });

    const connected = readFileSync(path, "utf8");
    // The comment and the user's own formatting are still present and unmoved.
    expect(connected).toContain("// my hand-written layout");
    expect(connected).toContain('"API_TIMEOUT_MS": "1200000",   /* keep */');
    expect(connected).toContain('"KEEP_ME": "yes"');
    // Keys appeared in the env block only.
    expect(connected).toContain('"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787"');
    // Untouched sections are byte-identical.
    expect(connected).toContain('"attribution": { "commit": "" }');
    // The file is still valid JSONC and the env block holds both ours and the user's keys.
    const envKeys = jsoncObjectKeys(connected, ["env"]);
    expect(envKeys).toContain("ANTHROPIC_BASE_URL");
    expect(envKeys).toContain("ANTHROPIC_AUTH_TOKEN");
    expect(envKeys).toContain("KEEP_ME");
    expect(envKeys).toContain("API_TIMEOUT_MS");
  });

  it("removes only its own env keys on restore, leaving comments intact", () => {
    const path = claudeCodeSettingsPath();
    writeFileSync(
      path,
      `{
  // settings I keep by hand
  "theme": "dark",
  "env": {
    "KEEP_ME": "yes"
  }
}
`,
    );
    applyClaudeCode({ port: 8787, models: ["jevonian/auto"] });
    restoreClaudeCode();

    const restored = readFileSync(path, "utf8");
    expect(restored).toContain("// settings I keep by hand");
    expect(restored).toContain('"KEEP_ME": "yes"');
    expect(restored).not.toContain("ANTHROPIC_BASE_URL");
    expect(restored).not.toContain("ANTHROPIC_AUTH_TOKEN");
  });
});
