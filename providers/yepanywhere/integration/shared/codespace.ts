import type { ProviderName } from "./types.js";

export const CODESPACE_PROVIDERS = [
  "claude",
  "claude-gateway",
  "claude-ollama",
  "codex",
  "codex-oss",
  "opencode",
  "pi",
] as const satisfies readonly ProviderName[];

export function isCodespaceProvider(name: string): boolean {
  return CODESPACE_PROVIDERS.some((provider) => provider === name);
}

export interface CodespaceEndpoint {
  port: number;
  label: string;
  visibility: "authenticated" | "public";
}

export interface CodespaceUSBDevice {
  source: string;
  busId: string;
  description: string;
  port?: number;
  state?: string;
}

export interface CodespaceUSBState {
  version: number;
  sources: { name: string; error?: string }[];
  devices: CodespaceUSBDevice[];
}

export interface CodespaceCapabilities {
  ports: boolean;
  usb: boolean;
  ssh?: { user: string; host: string; port: number };
}
