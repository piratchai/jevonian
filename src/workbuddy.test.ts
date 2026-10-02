import { mkdtempSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { afterEach, describe, expect, it } from "vite-plus/test";

import {
  ensureWorkbuddySystem,
  foldOpenAIChatStream,
  parseWorkbuddySession,
  workbuddyAuthHeaders,
  workbuddyClientHeaders,
  workbuddyPlainString,
  WORKBUDDY_SYSTEM_PROMPT,
  signInWorkbuddyAi,
  saveWorkbuddySession,
  readWorkbuddySession,
} from "./workbuddy";

describe("workbuddy", () => {
  const previousAuth = process.env.JEVONIAN_WORKBUDDY_AI_AUTH;

  afterEach(() => {
    if (previousAuth === undefined) delete process.env.JEVONIAN_WORKBUDDY_AI_AUTH;
    else process.env.JEVONIAN_WORKBUDDY_AI_AUTH = previousAuth;
  });

  it("reads plain string tokens and rejects encrypted envelopes", () => {
    expect(workbuddyPlainString("tok")).toBe("tok");
    expect(workbuddyPlainString({ $wbEncrypted: 1, envelope: "x" })).toBe("");
    expect(
      parseWorkbuddySession({
        account: { uid: "u1", nickname: { $wbEncrypted: 1, envelope: "n" } },
        auth: {
          accessToken: { $wbEncrypted: 1, envelope: "a" },
          refreshToken: { $wbEncrypted: 1, envelope: "r" },
          domain: "www.workbuddy.ai",
        },
      }),
    ).toBeUndefined();
  });

  it("parses a plaintext desktop session and a flat Jevonian session", () => {
    const desktop = parseWorkbuddySession({
      account: { uid: "uid-1", nickname: "Ada" },
      auth: {
        accessToken: "access",
        refreshToken: "refresh",
        expiresAt: 9_000_000_000_000,
        domain: "www.workbuddy.ai",
      },
    });
    expect(desktop).toMatchObject({
      uid: "uid-1",
      accessToken: "access",
      refreshToken: "refresh",
      nickname: "Ada",
      domain: "www.workbuddy.ai",
    });

    const flat = parseWorkbuddySession({
      uid: "uid-2",
      accessToken: "a2",
      refreshToken: "r2",
      expiresAt: 1,
      domain: "www.codebuddy.ai",
    });
    expect(flat).toMatchObject({ uid: "uid-2", accessToken: "a2", domain: "www.codebuddy.ai" });
  });

  it("builds WorkBuddy AI auth and client headers", () => {
    const auth = workbuddyAuthHeaders({
      uid: "u",
      accessToken: "tok",
      refreshToken: "",
      expiresAt: 0,
      refreshExpiresAt: 0,
      domain: "www.workbuddy.ai",
    });
    expect(auth.authorization).toBe("Bearer tok");
    expect(auth["x-user-id"]).toBe("u");
    expect(auth["x-domain"]).toBe("www.workbuddy.ai");
    expect(auth["x-product"]).toBe("SaaS");
    expect(auth["x-ide-type"]).toBe("WorkBuddy");
    expect(auth["user-agent"]).toMatch(/^WorkBuddy\//);

    const client = workbuddyClientHeaders("abc123");
    expect(client["x-requested-with"]).toBe("XMLHttpRequest");
    expect(client["x-agent-intent"]).toBe("craft");
    expect(client["x-ide-name"]).toBe("WorkBuddy");
    expect(client["x-conversation-id"]).toBe("abc123");
  });

  it("injects a system message when the first message is not system", () => {
    const withSystem = ensureWorkbuddySystem({
      messages: [
        { role: "system", content: "keep" },
        { role: "user", content: "hi" },
      ],
    });
    expect(withSystem.messages).toEqual([
      { role: "system", content: "keep" },
      { role: "user", content: "hi" },
    ]);

    const without = ensureWorkbuddySystem({
      messages: [{ role: "user", content: "hi" }],
    });
    expect(without.messages).toEqual([
      { role: "system", content: WORKBUDDY_SYSTEM_PROMPT },
      { role: "user", content: "hi" },
    ]);
  });

  it("folds an OpenAI chat SSE stream into one completion", async () => {
    const sse = [
      'data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}\n\n',
      'data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"lo"}}]}\n\n',
      'data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}\n\n',
      "data: [DONE]\n\n",
    ].join("");
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(sse));
        controller.close();
      },
    });
    const json = await foldOpenAIChatStream(stream, "primary-model");
    expect(json.id).toBe("chatcmpl-1");
    expect((json.choices as Array<{ message: { content: string } }>)[0]?.message.content).toBe(
      "Hello",
    );
    expect(json.usage).toMatchObject({ prompt_tokens: 2, completion_tokens: 1 });
  });

  it("reads and writes a Jevonian WorkBuddy session file", () => {
    const dir = mkdtempSync(join(tmpdir(), "jevonian-wb-"));
    const path = join(dir, "workbuddy-ai.json");
    process.env.JEVONIAN_WORKBUDDY_AI_AUTH = path;
    saveWorkbuddySession({
      uid: "u",
      accessToken: "a",
      refreshToken: "r",
      expiresAt: 9_000_000_000_000,
      refreshExpiresAt: 9_000_000_000_000,
      domain: "www.workbuddy.ai",
      nickname: "Ada",
    });
    expect(readWorkbuddySession()).toMatchObject({ uid: "u", accessToken: "a", nickname: "Ada" });
  });

  it("completes a Magpie-style browser sign-in against a mock server", async () => {
    const dir = mkdtempSync(join(tmpdir(), "jevonian-wb-signin-"));
    const path = join(dir, "session.json");
    process.env.JEVONIAN_WORKBUDDY_AI_AUTH = path;

    const server = createServer((req, res) => {
      const url = new URL(req.url ?? "/", "http://127.0.0.1");
      const ok = (data: unknown) => {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ code: 0, data }));
      };
      if (url.pathname === "/v2/plugin/auth/state") {
        expect(url.searchParams.get("platform")).toBe("workbuddy-ai");
        ok({
          state: "s1",
          authUrl: "https://www.workbuddy.ai/login?platform=workbuddy-ai&state=s1",
        });
        return;
      }
      if (url.pathname === "/v2/plugin/auth/token") {
        ok({
          accessToken: "access-1",
          refreshToken: "refresh-1",
          expiresIn: 3600,
          refreshExpiresIn: 7200,
          domain: "www.workbuddy.ai",
        });
        return;
      }
      if (url.pathname === "/v2/plugin/login/account") {
        ok({ uid: "uid-9", nickname: "Tester" });
        return;
      }
      res.writeHead(404);
      res.end();
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    if (!address || typeof address === "string") throw new Error("no port");
    const endpoint = `http://127.0.0.1:${address.port}`;
    try {
      const opened: string[] = [];
      const result = await signInWorkbuddyAi({
        endpoint,
        open: (url) => opened.push(url),
        pollMs: 5,
        timeoutMs: 5_000,
      });
      expect(opened[0]).toContain("https://www.workbuddy.ai/login");
      expect(result.user).toBe("Tester");
      expect(result.creds).toMatchObject({
        uid: "uid-9",
        accessToken: "access-1",
        refreshToken: "refresh-1",
        domain: "www.workbuddy.ai",
      });
      expect(readWorkbuddySession()?.accessToken).toBe("access-1");
    } finally {
      await new Promise<void>((resolve, reject) =>
        server.close((error) => (error ? reject(error) : resolve())),
      );
    }
  });

  it("ignores an encrypted desktop file written for coverage of the path", () => {
    const dir = mkdtempSync(join(tmpdir(), "jevonian-wb-enc-"));
    const path = join(dir, "workbuddy-desktop-ai.info");
    writeFileSync(
      path,
      JSON.stringify({
        account: { uid: "u" },
        auth: {
          accessToken: { $wbEncrypted: 1, envelope: "x" },
          refreshToken: { $wbEncrypted: 1, envelope: "y" },
        },
      }),
    );
    process.env.JEVONIAN_WORKBUDDY_AI_AUTH = path;
    expect(readWorkbuddySession()).toBeUndefined();
  });

  it("does not fall back to the desktop file when an explicit session path is set", () => {
    const dir = mkdtempSync(join(tmpdir(), "jevonian-wb-explicit-"));
    const missing = join(dir, "missing.json");
    process.env.JEVONIAN_WORKBUDDY_AI_AUTH = missing;
    // Even if a desktop file existed, an explicit path that fails must not leak into it.
    expect(readWorkbuddySession()).toBeUndefined();
    expect(readWorkbuddySession({ credentialsPath: missing })).toBeUndefined();
  });
});
