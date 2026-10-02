import {
  Antigravity,
  Anthropic,
  Claude,
  CodeBuddy,
  CommandCode,
  Cursor,
  DeepSeek,
  Devin,
  Google,
  Groq,
  LmStudio,
  Minimax,
  Mistral,
  Moonshot,
  Ollama,
  OpenAI,
  OpenCode,
  OpenRouter,
  Qwen,
  XAI,
  Zhipu,
} from "@lobehub/icons";
import type { ComponentType, SVGProps } from "react";

import orcarouter from "@/assets/logos/orcarouter.png";

type IconProps = SVGProps<SVGSVGElement> & { size?: string | number };
type LobeIcon = ComponentType<IconProps> & {
  Color?: ComponentType<IconProps>;
  title?: string;
};

/** Prefer the official color mark when Lobe ships one; otherwise the mono glyph. */
function brandIcon(icon: LobeIcon): ComponentType<IconProps> {
  return icon.Color ?? icon;
}

/**
 * Map Jevonian provider / preset / client logo ids onto @lobehub/icons components.
 * Keys cover both short ids (`openai`) and preset names (`chatgpt-subscription`).
 */
export const PROVIDER_ICONS: Record<string, ComponentType<IconProps>> = {
  anthropic: brandIcon(Anthropic),
  antigravity: brandIcon(Antigravity),
  claude: brandIcon(Claude),
  "claude-subscription": brandIcon(Claude),
  commandcode: brandIcon(CommandCode),
  cursor: brandIcon(Cursor),
  "cursor-subscription": brandIcon(Cursor),
  deepseek: brandIcon(DeepSeek),
  devin: brandIcon(Devin),
  "devin-subscription": brandIcon(Devin),
  google: brandIcon(Google),
  groq: brandIcon(Groq),
  lmstudio: brandIcon(LmStudio),
  minimax: brandIcon(Minimax),
  mistral: brandIcon(Mistral),
  moonshotai: brandIcon(Moonshot),
  ollama: brandIcon(Ollama),
  openai: brandIcon(OpenAI),
  "chatgpt-subscription": brandIcon(OpenAI),
  opencode: brandIcon(OpenCode),
  "opencode-go": brandIcon(OpenCode),
  "opencode-zen": brandIcon(OpenCode),
  openrouter: brandIcon(OpenRouter),
  qwen: brandIcon(Qwen),
  // WorkBuddy AI (international) shares the CodeBuddy mark in Lobe's set.
  workbuddy: brandIcon(CodeBuddy),
  "workbuddy-ai": brandIcon(CodeBuddy),
  "workbuddy-ai-subscription": brandIcon(CodeBuddy),
  xai: brandIcon(XAI),
  zai: brandIcon(Zhipu),
};

/** Raster fallbacks for brands Lobe does not ship yet. */
export const PROVIDER_IMAGES: Record<string, string> = {
  orcarouter,
};
