import assert from "node:assert/strict";
import test from "node:test";

import { assignProviderModels } from "../web/src/lib/provider-assignment.ts";

const route = (id, models = [], providers) => ({
  id,
  label: id,
  description: "",
  models,
  ...(providers ? { providers } : {}),
});
const state = (saved, derived = saved) => ({
  config: { routing: { routings: saved } },
  routings: derived,
});

test("source assignment appends backups and preserves unrelated tasks", () => {
  const other = route("chat", ["chat-model"]);
  const result = assignProviderModels(
    state([route("plan", ["first", "second"]), other]),
    ["plan"],
    ["second", "backup"],
    "new",
    false,
  );
  assert.deepEqual(result[0].models, ["first", "second", "backup"]);
  assert.equal(result[1], other);
});

test("source assignment respects empty and ordered allow-lists", () => {
  const providers = { blocked: [], excluded: ["old"], allowed: ["old", "new"] };
  const result = assignProviderModels(
    state([route("plan", ["first"], providers)]),
    ["plan"],
    ["blocked", "excluded", "allowed", "implicit"],
    "new",
    false,
  );
  assert.deepEqual(result[0].models, ["first", "allowed", "implicit"]);
  assert.equal(result[0].providers, providers);
});

test("auto-derived assignment needs explicit conversion and keeps derived order", () => {
  const input = state(
    [route("plan"), route("chat")],
    [route("plan", ["first", "second"]), route("chat", ["chat-model"])],
  );
  assert.throws(
    () => assignProviderModels(input, ["plan"], ["backup"], "new", false),
    /Confirm conversion/,
  );
  const result = assignProviderModels(input, ["plan"], ["backup"], "new", true);
  assert.deepEqual(result[0].models, ["first", "second", "backup"]);
  assert.deepEqual(result[1].models, []);
});

test("canonical assignment respects exclusions saved under raw variant ids", () => {
  const input = state([route("plan", ["first"], { "gpt-5.6": [] })]);
  const result = assignProviderModels(
    { ...input, canonicals: [{ id: "gpt-5-6", variants: [{ provider: "new", model: "gpt-5.6" }] }] },
    ["plan"],
    ["gpt-5-6"],
    "new",
    false,
  );
  assert.deepEqual(result[0].models, ["first"]);
});

test("routing-only retries do not duplicate backup models", () => {
  const first = assignProviderModels(
    state([route("plan", ["first"])]),
    ["plan"],
    ["backup"],
    "new",
    false,
  );
  const retry = assignProviderModels(state(first), ["plan"], ["backup"], "new", false);
  assert.deepEqual(retry, first);
});
