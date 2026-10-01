import { describe, expect, it } from "vite-plus/test";

import { applyJsoncEdits } from "./jsonc";

describe("applyJsoncEdits", () => {
  it("replaces a top-level scalar value in place, preserving the rest", () => {
    const text = `{
  // user comment stays
  "model": "old-model",
  "other":   123
}
`;
    const out = applyJsoncEdits(text, [{ path: ["model"], value: "new-model" }]);
    expect(out).toBe(`{
  // user comment stays
  "model": "new-model",
  "other":   123
}
`);
  });

  it("preserves weird spacing and comments around untouched keys", () => {
    const text = `{
    "a":1,    /* inline */
    "b" : "x"
}`;
    const out = applyJsoncEdits(text, [{ path: ["a"], value: 2 }]);
    expect(out).toBe(`{
    "a":2,    /* inline */
    "b" : "x"
}`);
  });

  it("sets a nested member value inside an object", () => {
    const text = `{
  "env": {
    "ANTHROPIC_BASE_URL": "http://old",
    "KEEP": "me"
  }
}
`;
    const out = applyJsoncEdits(text, [
      { path: ["env", "ANTHROPIC_BASE_URL"], value: "http://127.0.0.1:8787" },
    ]);
    expect(out).toBe(`{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787",
    "KEEP": "me"
  }
}
`);
  });

  it("inserts a missing member into an existing object with matching indent", () => {
    const text = `{
    "env": {
        "EXISTING": "1"
    }
}`;
    const out = applyJsoncEdits(text, [{ path: ["env", "NEW_KEY"], value: "v" }]);
    expect(out).toBe(`{
    "env": {
        "EXISTING": "1",
        "NEW_KEY": "v"
    }
}`);
  });

  it("inserts a missing member into an empty object", () => {
    const text = `{
  "env": {}
}`;
    const out = applyJsoncEdits(text, [{ path: ["env", "K"], value: "v" }]);
    expect(out).toContain('"env"');
    expect(out).toContain('"K": "v"');
  });

  it("inserts a top-level member at the end of the root object", () => {
    const text = `{
  "a": 1
}`;
    const out = applyJsoncEdits(text, [{ path: ["b"], value: "two" }]);
    expect(out).toBe(`{
  "a": 1,
  "b": "two"
}`);
  });

  it("removes a member and its trailing comma", () => {
    const text = `{
  "a": 1,
  "b": 2,
  "c": 3
}`;
    const out = applyJsoncEdits(text, [{ path: ["a"], value: undefined }]);
    expect(out).toBe(`{
  "b": 2,
  "c": 3
}`);
  });

  it("removes the last member and the preceding comma", () => {
    const text = `{
  "a": 1,
  "b": 2
}`;
    const out = applyJsoncEdits(text, [{ path: ["b"], value: undefined }]);
    expect(out).toBe(`{
  "a": 1
}`);
  });

  it("does not touch array values or commented-out keys", () => {
    const text = `{
  // "model": "ghost",
  "list": [1, 2, {"model": "inner"}],
  "model": "real"
}`;
    const out = applyJsoncEdits(text, [{ path: ["model"], value: "new" }]);
    expect(out).toBe(`{
  // "model": "ghost",
  "list": [1, 2, {"model": "inner"}],
  "model": "new"
}`);
  });

  it("handles escaped characters in keys and string values", () => {
    const text = `{
  "ke\\"y": "old",
  "path": "C:\\\\x"
}`;
    const out = applyJsoncEdits(text, [{ path: ["path"], value: "C:\\y" }]);
    expect(out).toContain('"path": "C:\\\\y"');
    expect(out).toContain('"ke\\"y": "old"');
  });

  it("writes a whole env block surgically, leaving siblings alone", () => {
    const text = `{
  "model": "m",
  "env": {
    "ANTHROPIC_BASE_URL": "http://x",
    "USER_KEY": "stay" // keep me
  },
  "attribution": { "commit": "" }
}`;
    const out = applyJsoncEdits(text, [
      { path: ["env", "ANTHROPIC_BASE_URL"], value: "http://127.0.0.1:8787" },
      { path: ["env", "ANTHROPIC_AUTH_TOKEN"], value: "tok" },
      { path: ["env", "STALE"], value: undefined },
    ]);
    expect(out).toContain('"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787"');
    expect(out).toContain('"ANTHROPIC_AUTH_TOKEN": "tok"');
    expect(out).toContain('"USER_KEY": "stay" // keep me');
    expect(out).toContain('"attribution": { "commit": "" }');
  });

  it("skips a path that cannot be resolved rather than corrupting the file", () => {
    const text = `{ "a": 1 }`;
    const out = applyJsoncEdits(text, [
      { path: ["env", "NESTED", "DEEP"], value: "x" }, // env does not exist
    ]);
    expect(out).toBe(`{ "a": 1 }`);
  });

  it("round-trips to identical JSON for strict files", () => {
    const text = `{"a":1,"b":{"c":2}}`;
    const out = applyJsoncEdits(text, [{ path: ["b", "c"], value: 3 }]);
    expect(JSON.parse(out)).toEqual({ a: 1, b: { c: 3 } });
  });
});
