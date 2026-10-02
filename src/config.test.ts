import { describe, expect, it } from "vite-plus/test";

import { MODEL_SYNC_DEFAULT_SOURCES, parseConfig, providerSyncsModels } from "./config";

describe("provider parsing", () => {
  it("accepts type and oauthSource devin", () => {
    const config = parseConfig({
      providers: [
        {
          name: "devin-subscription",
          type: "devin",
          baseUrl: "https://server.codeium.com",
          auth: "oauth",
          oauthSource: "devin",
          billing: "subscription",
          models: ["swe-1-6-slow"],
        },
      ],
    });
    const provider = config.providers[0];
    expect(provider?.type).toBe("devin");
    expect(provider?.auth).toBe("oauth");
    expect(provider?.oauthSource).toBe("devin");
    expect(provider?.billing).toBe("subscription");
    expect(provider && providerSyncsModels(provider)).toBe(true);
    expect(MODEL_SYNC_DEFAULT_SOURCES).toContain("devin");
  });

  it("accepts oauthSource workbuddy-ai", () => {
    const config = parseConfig({
      providers: [
        {
          name: "workbuddy-ai-subscription",
          type: "openai",
          baseUrl: "https://www.workbuddy.ai/v2",
          auth: "oauth",
          oauthSource: "workbuddy-ai",
          billing: "subscription",
          models: ["primary-model"],
        },
      ],
    });
    const provider = config.providers[0];
    expect(provider?.oauthSource).toBe("workbuddy-ai");
    expect(provider?.billing).toBe("subscription");
    expect(provider && providerSyncsModels(provider)).toBe(true);
    expect(MODEL_SYNC_DEFAULT_SOURCES).toContain("workbuddy-ai");
  });

  it("still accepts antigravity with the gemini type", () => {
    const config = parseConfig({
      providers: [
        {
          name: "antigravity",
          type: "gemini",
          baseUrl: "https://daily-cloudcode-pa.googleapis.com",
          auth: "oauth",
          oauthSource: "antigravity",
          models: [],
        },
      ],
    });
    expect(config.providers[0]?.type).toBe("gemini");
    expect(config.providers[0]?.oauthSource).toBe("antigravity");
  });

  it("drops unknown oauth sources and rejects unknown types", () => {
    const config = parseConfig({
      providers: [
        {
          name: "p",
          type: "openai",
          baseUrl: "https://example.com/v1",
          auth: "oauth",
          oauthSource: "nope",
        },
      ],
    });
    expect(config.providers[0]?.oauthSource).toBeUndefined();
    expect(() =>
      parseConfig({ providers: [{ name: "p", type: "grpc", baseUrl: "https://example.com" }] }),
    ).toThrow(/"devin", or "cursor"/);
  });
});
