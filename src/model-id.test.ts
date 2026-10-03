import { describe, expect, it } from "vite-plus/test";

import { bareModelId, modelVendor } from "./model-id";

describe("bareModelId", () => {
  it("keeps an unprefixed id", () => {
    expect(bareModelId("deepseek-v4.1-flash")).toBe("deepseek-v4.1-flash");
  });

  it("drops a vendor prefix", () => {
    expect(bareModelId("openai/gpt-6-astra")).toBe("gpt-6-astra");
  });

  it("drops a nested path down to the last segment", () => {
    expect(bareModelId("accounts/fireworks/models/kimi-k3")).toBe("kimi-k3");
  });

  it("keeps the id when the segment after the slash is empty", () => {
    expect(bareModelId("vendor/")).toBe("vendor/");
  });
});

describe("modelVendor", () => {
  it("reads the first segment", () => {
    expect(modelVendor("deepseek/deepseek-flash")).toBe("deepseek");
    expect(modelVendor("accounts/fireworks/models/kimi-k3")).toBe("accounts");
  });

  it("is empty without a prefix", () => {
    expect(modelVendor("gpt-6")).toBe("");
    expect(modelVendor("/leading")).toBe("");
  });
});
