import assert from "node:assert/strict";
import test from "node:test";

import {
  providerAccountLabel,
  providerBrandId,
  providerDisplayName,
  resolveProviderIdentity,
} from "../web/src/lib/provider-name.ts";

test("brand comes from the name for a plain provider", () => {
  assert.equal(providerBrandId("deepseek"), "deepseek");
  assert.equal(providerDisplayName("deepseek"), "DeepSeek");
  assert.equal(providerAccountLabel("deepseek"), undefined);
});

test("a second-account name keeps its brand and exposes the account", () => {
  const identity = resolveProviderIdentity("claude-work");
  assert.equal(identity.brand, "claude-subscription");
  assert.equal(identity.name, "Claude");
  assert.equal(identity.account, "work");
  assert.equal(identity.isSecondAccount, true);
});

test("the longest alias wins so opencode-go is not read as opencode", () => {
  assert.equal(providerBrandId("opencode-go"), "opencode-go");
  assert.equal(providerBrandId("opencode-zen"), "opencode-zen");
  assert.equal(providerDisplayName("opencode-go"), "OpenCode Go");
  // A bare `opencode` still resolves to the shared mark.
  assert.equal(providerBrandId("opencode"), "opencode");
});

test("oauthSource decides the brand even when the name does not match", () => {
  const identity = resolveProviderIdentity({
    name: "acme-prod",
    oauthSource: "codex",
    type: "responses",
  });
  assert.equal(identity.brand, "chatgpt-subscription");
  assert.equal(identity.name, "ChatGPT (Codex)");
  // No login and no brand token in the name: no account is invented.
  assert.equal(identity.isSecondAccount, false);
});

test("the user-chosen name outranks the derived sign-in location", () => {
  // A provider named for its account must read that account, not the home directory it points
  // at. `codex-personal` points at `~/.codex-work`, and must still read `personal`.
  const identity = resolveProviderIdentity({
    name: "codex-personal",
    oauthSource: "codex",
    login: { home: "~/.codex-work" },
  });
  assert.equal(identity.brand, "chatgpt-subscription");
  assert.equal(identity.account, "personal");
  assert.equal(identity.accountDetail, "~/.codex-work");
  assert.equal(identity.isSecondAccount, true);
});

test("the derived location drops the brand token and the dot", () => {
  // The name carries no account of its own, so the location supplies the label: `~/.claude-work`
  // reads `work`, without the dot or the repeated brand.
  const identity = resolveProviderIdentity({
    name: "claude-subscription",
    oauthSource: "claude-code",
    login: { home: "~/.claude-work" },
  });
  assert.equal(identity.brand, "claude-subscription");
  assert.equal(identity.account, "work");
  assert.equal(identity.accountDetail, "~/.claude-work");
  assert.equal(identity.isSecondAccount, true);
});

test("an explicit login label outranks both the name and the location", () => {
  const identity = resolveProviderIdentity({
    name: "claude-work",
    oauthSource: "claude-code",
    login: { label: "day-job", home: "~/.claude-work" },
  });
  assert.equal(identity.account, "day-job");
});

test("a credential file loses its extension in the account label", () => {
  const identity = resolveProviderIdentity({
    name: "codex",
    oauthSource: "codex",
    login: { credentialsPath: "/tmp/team/auth.json" },
  });
  assert.equal(identity.account, "auth");
});

test("a brand token inside the name reads as the account", () => {
  assert.equal(resolveProviderIdentity("cursor-personal").account, "personal");
  assert.equal(resolveProviderIdentity("opencode-go-team").account, "team");
});

test("an unknown provider is humanized and keeps its own name as the brand", () => {
  const identity = resolveProviderIdentity("my-gateway");
  assert.equal(identity.brand, "my-gateway");
  assert.equal(identity.name, "My Gateway");
  assert.equal(identity.isSecondAccount, false);
});

test("the wire is a last resort for a brand-less name", () => {
  assert.equal(providerBrandId({ name: "acme", type: "cursor" }), "cursor-subscription");
  assert.equal(providerBrandId({ name: "acme", type: "devin" }), "devin-subscription");
});

test("the display-name helpers agree with the full resolver", () => {
  assert.equal(providerDisplayName("claude-work"), resolveProviderIdentity("claude-work").name);
  assert.equal(providerBrandId("grok-team"), "xai");
  assert.equal(providerAccountLabel("grok-team"), "team");
});
