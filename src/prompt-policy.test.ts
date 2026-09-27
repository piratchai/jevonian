import { describe, expect, it } from "vite-plus/test";

import {
  BUILTIN_PROMPT_REWRITES,
  DEFAULT_PROMPT_POLICY,
  parsePromptPolicy,
  rewritePromptBodies,
  sanitizeBuiltinPrompt,
  sanitizePromptWithPolicy,
} from "./prompt-policy";

const IDENTITY = "You operate in Cursor.";
const TOOL_CALLING =
  "Use specialized tools instead of terminal commands when possible, as this provides a " +
  "better user experience. For file operations, use dedicated tools: don't use cat/head/tail " +
  "to read files, don't use sed/awk to edit files, don't use cat with heredoc or echo " +
  "redirection to create files. Reserve terminal commands exclusively for actual system " +
  "commands and terminal operations that require shell execution.";
const METHOD_HEADING =
  "## METHOD 2: MARKDOWN CODE BLOCKS - Proposing or Displaying Code NOT already in Codebase";
const TERMINAL_SENTENCE = "There is one text file for each terminal the user has running.";

describe("prompt policy built-ins", () => {
  it("removes every observed rival-prompt signature", () => {
    const out = sanitizeBuiltinPrompt(
      [IDENTITY, TOOL_CALLING, METHOD_HEADING, TERMINAL_SENTENCE].join("\n\n"),
    );
    for (const [from] of BUILTIN_PROMPT_REWRITES) expect(out).not.toContain(from);
    expect(out).not.toContain("You operate in Cursor");
  });

  it("keeps unrelated text untouched", () => {
    const text = "You are an AI coding assistant.\n\nAlways run the tests before claiming success.";
    expect(sanitizeBuiltinPrompt(text)).toBe(text);
  });

  it("rewrites the identity line in suffixed and cased forms", () => {
    expect(sanitizeBuiltinPrompt("You operate in Cursor IDE")).toBe(
      "You work inside the user's code editor.",
    );
    expect(sanitizeBuiltinPrompt("you operate in cursor.")).toBe(
      "You work inside the user's code editor.",
    );
  });

  it("is idempotent", () => {
    const once = sanitizeBuiltinPrompt([IDENTITY, TOOL_CALLING].join("\n\n"));
    expect(sanitizeBuiltinPrompt(once)).toBe(once);
  });

  it("leaves the prompt alone when built-ins are disabled", () => {
    const text = IDENTITY;
    expect(sanitizePromptWithPolicy(text, { builtins: false, rewrites: [] })).toBe(text);
  });
});

describe("operator rules", () => {
  it("applies a plain match and replacement", () => {
    const policy = parsePromptPolicy({
      rewrites: [{ match: "acme-corp-secret", replace: "ACME" }],
    });
    expect(sanitizePromptWithPolicy("deploy acme-corp-secret now", policy)).toBe("deploy ACME now");
  });

  it("supports group references and flags", () => {
    const policy = parsePromptPolicy({
      rewrites: [{ match: "ticket-(\\d+)", replace: "TICKET $1", flags: "i" }],
    });
    expect(sanitizePromptWithPolicy("see TICKET-42", policy)).toBe("see TICKET 42");
  });

  it("deletes when the replacement is empty", () => {
    const policy = parsePromptPolicy({ rewrites: [{ match: "\\s*REDACTME\\s*", replace: "" }] });
    expect(sanitizePromptWithPolicy("a REDACTME b", policy)).toBe("ab");
  });

  it("runs after the built-ins", () => {
    const policy = parsePromptPolicy({
      rewrites: [{ match: "user's code editor", replace: "IDE" }],
    });
    expect(sanitizePromptWithPolicy(IDENTITY, policy)).toBe("You work inside the IDE.");
  });

  it("drops rules that would not compile instead of throwing", () => {
    const policy = parsePromptPolicy({
      rewrites: [
        { match: "([unclosed", replace: "x" },
        { match: "", replace: "x" },
        { match: "ok", replace: 5 },
        { match: "good", replace: "fine" },
      ],
    });
    expect(policy.rewrites).toEqual([{ match: "good", replace: "fine" }]);
    expect(sanitizePromptWithPolicy("ok good ([unclosed", policy)).toBe("ok fine ([unclosed");
  });

  it("filters unknown flags rather than failing", () => {
    const policy = parsePromptPolicy({ rewrites: [{ match: "x", replace: "y", flags: "qg" }] });
    expect(policy.rewrites[0]?.flags).toBe("g");
    expect(sanitizePromptWithPolicy("x x", policy)).toBe("y y");
  });

  it("defaults to built-ins on with no rules", () => {
    expect(parsePromptPolicy(undefined)).toEqual(DEFAULT_PROMPT_POLICY);
    expect(parsePromptPolicy({ builtins: false }).builtins).toBe(false);
  });
});

describe("rewritePromptBodies", () => {
  const policy = parsePromptPolicy({});

  it("rewrites Chat Completions system messages and leaves user messages alone", () => {
    const body = {
      model: "m",
      messages: [
        { role: "system", content: IDENTITY },
        { role: "user", content: IDENTITY },
      ],
    };
    const out = rewritePromptBodies(body, policy);
    expect(out.messages[0]?.content).toBe("You work inside the user's code editor.");
    expect(out.messages[1]?.content).toBe(IDENTITY);
    // The caller's body is never mutated.
    expect(body.messages[0]?.content).toBe(IDENTITY);
  });

  it("rewrites developer messages and text blocks", () => {
    const out = rewritePromptBodies(
      {
        messages: [
          { role: "developer", content: [{ type: "text", text: IDENTITY }] },
          { role: "system", content: [{ type: "image_url", image_url: { url: "u" } }] },
        ],
      },
      policy,
    );
    expect(out.messages?.[0]?.content).toEqual([
      { type: "text", text: "You work inside the user's code editor." },
    ]);
    // A block without text is passed through untouched.
    expect(out.messages?.[1]?.content).toEqual([{ type: "image_url", image_url: { url: "u" } }]);
  });

  it("rewrites Anthropic system strings and blocks plus Responses instructions", () => {
    const anthropic = rewritePromptBodies({ system: IDENTITY, messages: [] }, policy);
    expect(anthropic.system).toBe("You work inside the user's code editor.");

    const blocks = rewritePromptBodies(
      { system: [{ type: "text", text: IDENTITY }], messages: [] },
      policy,
    );
    expect(blocks.system).toEqual([
      { type: "text", text: "You work inside the user's code editor." },
    ]);

    const responses = rewritePromptBodies({ instructions: IDENTITY, input: [] }, policy);
    expect(responses.instructions).toBe("You work inside the user's code editor.");
  });

  it("returns the body unchanged when nothing matches", () => {
    const body = { messages: [{ role: "system", content: "plain" }] };
    expect(rewritePromptBodies(body, policy)).toBe(body);
  });
});
