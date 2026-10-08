import assert from "node:assert/strict";
import test from "node:test";

import {
  allowedProviders,
  collectProvidersByModel,
  mergeRoutingDrafts,
  routeSavePayload,
  validRoutingId,
  BUILTIN_ROUTING_IDS,
} from "../web/src/pages/routing-state.ts";

const route = (id, models = []) => ({ id, label: id, description: "", models });

test("implicit providers discover all, explicit empty providers withhold the model", () => {
  assert.deepEqual(allowedProviders(["a", "b"], undefined), ["a", "b"]);
  assert.deepEqual(allowedProviders(["a", "b"], []), []);
  assert.deepEqual(allowedProviders(["a", "b"], ["b", "missing", "a"]), ["b", "a"]);
});

test("refresh retains dirty drafts but updates clean routes and discovery", () => {
  const previous = [route("plan"), route("chat")];
  const draft = { ...route("plan"), description: "Local edit" };
  const fresh = [route("plan", ["canonical/model"]), route("chat", ["other/model"])];
  assert.deepEqual(mergeRoutingDrafts(fresh, [draft, route("chat"), route("new")], previous), [
    draft,
    fresh[1],
    route("new"),
  ]);
});

test("saving one route cannot commit other drafts or pin automatic models", () => {
  const persisted = [route("plan"), route("chat")];
  const edited = { ...route("plan"), description: "Changed description" };
  assert.deepEqual(routeSavePayload(persisted, edited), [edited, persisted[1]]);
  assert.deepEqual(routeSavePayload(persisted, route("new")), [...persisted, route("new")]);
  assert.deepEqual(routeSavePayload(persisted, edited)[0].models, []);
});

test("provider list keeps canonical ids and explicit blocks through saves", () => {
  const edited = { ...route("plan", ["canonical/model"]), providers: { "canonical/model": [] } };
  assert.deepEqual(routeSavePayload([route("plan")], edited)[0], edited);
});

test("routing ids reject reserved and invalid aliases and identify protected tasks", () => {
  assert.equal(validRoutingId("frontend-2"), true);
  for (const id of ["auto", "A", "two words", "2first", ""])
    assert.equal(validRoutingId(id), false);
  for (const id of ["plan", "execute", "utility", "chat"])
    assert.equal(BUILTIN_ROUTING_IDS.has(id), true);
});

test("collectProvidersByModel indexes canonical variants across different model spellings", () => {
  const models = [
    { id: "deepseek-v4.1-flash", provider: "deepseek", configured: true },
    { id: "deepseek/deepseek-v4.1-flash", provider: "commandcode2", configured: true },
  ];
  const canonicals = [
    {
      id: "deepseek-v4-1-flash",
      variants: [
        { provider: "deepseek", model: "deepseek-v4.1-flash" },
        { provider: "commandcode2", model: "deepseek/deepseek-v4.1-flash" },
      ],
    },
  ];
  const map = collectProvidersByModel(models, canonicals);
  // Bare model spelling sees both providers
  assert.deepEqual(map.get("deepseek-v4.1-flash"), ["deepseek", "commandcode2"]);
  // Namespaced spelling sees both providers
  assert.deepEqual(map.get("deepseek/deepseek-v4.1-flash"), ["commandcode2", "deepseek"]);
  // Canonical ID sees both providers
  assert.deepEqual(map.get("deepseek-v4-1-flash"), ["deepseek", "commandcode2"]);
});

