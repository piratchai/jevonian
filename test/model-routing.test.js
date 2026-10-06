import assert from "node:assert/strict";
import test from "node:test";

import { assignProviderModels } from "../web/src/lib/provider-assignment.ts";
import {
  allowedProviders,
  mergeRoutingDrafts,
  routeSavePayload,
  validRoutingId,
} from "../web/src/pages/routing-state.ts";

const route = (id, models = ["model-a"], providers) => ({
  id,
  label: id,
  description: "Test task",
  models,
  ...(providers ? { providers } : {}),
});
const state = (routings, derived = routings) => ({
  config: { routing: { routings } },
  routings: derived,
});

test("provider assignment appends backups and keeps existing order", () => {
  const original = route("execute", ["model-a", "model-b"]);
  const other = route("chat");
  const result = assignProviderModels(
    state([original, other]),
    ["execute"],
    ["model-b", "model-c"],
    "new-source",
    false,
  );
  assert.deepEqual(result[0].models, ["model-a", "model-b", "model-c"]);
  assert.equal(result[1], other);
  assert.deepEqual(original.models, ["model-a", "model-b"]);
});

test("provider assignment respects explicit exclusions and empty allow-lists", () => {
  const original = route("execute", ["existing"], {
    blocked: [],
    excluded: ["other"],
    allowed: ["new-source"],
  });
  const result = assignProviderModels(
    state([original]),
    ["execute"],
    ["blocked", "excluded", "allowed", "implicit"],
    "new-source",
    false,
  );
  assert.deepEqual(result[0].models, ["existing", "allowed", "implicit"]);
  assert.deepEqual(result[0].providers, original.providers);
});

test("auto-derived task requires explicit conversion before assignment", () => {
  const snapshot = state([route("utility", [])], [route("utility", ["derived"])]);
  assert.throws(
    () => assignProviderModels(snapshot, ["utility"], ["backup"], "source", false),
    /Confirm conversion/,
  );
  assert.deepEqual(
    assignProviderModels(snapshot, ["utility"], ["backup"], "source", true)[0].models,
    ["derived", "backup"],
  );
});

test("undefined source policy allows discovery; empty policy withholds", () => {
  assert.deepEqual(allowedProviders(["a", "b"], undefined), ["a", "b"]);
  assert.deepEqual(allowedProviders(["a", "b"], []), []);
  assert.deepEqual(allowedProviders(["a", "b"], ["b", "missing", "b", "a"]), ["b", "a"]);
});

test("saving one task does not save another task draft", () => {
  const persisted = [route("plan"), route("execute")];
  const edited = route("execute", ["replacement"]);
  const result = routeSavePayload(persisted, edited);
  assert.equal(result[0], persisted[0]);
  assert.equal(result[1], edited);
  assert.deepEqual(persisted[1].models, ["model-a"]);
});

test("refresh merges server changes but preserves unsaved task edits", () => {
  const previous = [route("plan"), route("execute")];
  const edited = route("execute", ["draft"]);
  const added = route("frontend");
  const fresh = [route("plan", ["server"]), route("execute", ["remote"])];
  const merged = mergeRoutingDrafts(fresh, [previous[0], edited, added], previous);
  assert.equal(merged[0], fresh[0]);
  assert.equal(merged[1], edited);
  assert.equal(merged[2], added);
});

test("task aliases reject reserved and malformed IDs", () => {
  for (const id of ["plan", "frontend", "task-2"]) assert.equal(validRoutingId(id), true);
  for (const id of ["auto", "", "2task", "Task", "task name", "a".repeat(65)])
    assert.equal(validRoutingId(id), false);
});
