// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
import type {
  CodespaceBackend,
  CodespaceConnection,
  CodespaceConnections,
  ModelInfo,
} from "@yep-anywhere/shared";
import type { AuthStatus } from "./providers/types.js";

export function codespaceConnection(
  backend: string,
): CodespaceConnection | undefined {
  // The launcher validates and normalizes this configuration before startup.
  const connections: CodespaceConnections = JSON.parse(
    process.env.CODESPACE_YEP_CONNECTIONS || "{}",
  );
  return connections[backend as CodespaceBackend] ?? undefined;
}

export function codespaceAuthStatus(
  backend: string,
  installed: boolean,
): AuthStatus | undefined {
  const connection = codespaceConnection(backend);
  if (!connection) return undefined;
  const authenticated =
    installed &&
    (!connection.apiKeyEnv || !!process.env[connection.apiKeyEnv]?.trim());
  return { installed, authenticated, enabled: authenticated };
}

export function codespaceModels(backend: string): ModelInfo[] | undefined {
  const connection = codespaceConnection(backend);
  return connection?.models.map((model) => ({
    id:
      backend === "opencode" || backend === "pi"
        ? `codespace/${model.id}`
        : model.id,
    name: model.name ?? model.id,
    contextWindow: model.contextWindow,
    isDefault: model.id === connection.defaultModel,
  }));
}

export function codespaceModel(
  backend: string,
  selected?: string,
): string | undefined {
  const connection = codespaceConnection(backend);
  if (!connection) return selected;
  const prefix = backend === "opencode" || backend === "pi" ? "codespace/" : "";
  const defaultModel = connection.defaultModel
    ? `${prefix}${connection.defaultModel}`
    : undefined;
  const model = selected && selected !== "default" ? selected : defaultModel;
  if (connection.models.length) {
    if (!connection.models.some((entry) => `${prefix}${entry.id}` === model))
      throw new Error(
        `connections.${backend}.models: selected model is not in the configured catalog`,
      );
  } else if (prefix && !model?.startsWith(prefix)) {
    throw new Error(
      `connections.${backend}.models: select a codespace model before starting a session`,
    );
  }
  return model;
}

// Scope credentials to the child, retaining its native HOME and tool discovery.
// Read the named secret from the current server environment, not persisted state
// or a session's remoteEnv (which may belong to an earlier launch).
export function codespaceEnvironment(
  backend: string,
  inherited: NodeJS.ProcessEnv,
): NodeJS.ProcessEnv {
  const connection = codespaceConnection(backend);
  if (!connection) return inherited;
  const env = { ...inherited };
  env.CODESPACE_YEP_CONNECTIONS = JSON.stringify({ [backend]: connection });
  const key = connection.apiKeyEnv
    ? process.env[connection.apiKeyEnv]
    : undefined;
  if (connection.apiKeyEnv && !key?.trim())
    throw new Error(
      `connections.${backend}.apiKeyEnv: environment variable ${connection.apiKeyEnv} is missing or empty`,
    );
  if (connection.apiKeyEnv) env[connection.apiKeyEnv] = key;
  if (backend === "claude") {
    for (const name of [
      "ANTHROPIC_API_KEY",
      "ANTHROPIC_AUTH_TOKEN",
      "CLAUDE_CODE_OAUTH_TOKEN",
      "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR",
      "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
      "CLAUDE_CODE_USE_BEDROCK",
      "CLAUDE_CODE_USE_VERTEX",
      "CLAUDE_CODE_USE_FOUNDRY",
    ])
      env[name] = "";
    env.ANTHROPIC_BASE_URL = connection.baseUrl;
    // Claude requires a non-empty credential even for an unauthenticated API.
    env[
      connection.authMode === "bearer"
        ? "ANTHROPIC_AUTH_TOKEN"
        : "ANTHROPIC_API_KEY"
    ] = key ?? "codespace-no-auth";
  } else if (backend === "opencode") {
    let config: Record<string, unknown> = {};
    try {
      config = JSON.parse(env.OPENCODE_CONFIG_CONTENT || "{}");
      if (!config || typeof config !== "object" || Array.isArray(config))
        throw new Error();
    } catch {
      throw new Error("OPENCODE_CONFIG_CONTENT must be a JSON object");
    }
    const npm = {
      "openai-completions": "@ai-sdk/openai-compatible",
      "openai-responses": "@ai-sdk/openai",
      "anthropic-messages": "@ai-sdk/anthropic",
    }[connection.api ?? "openai-completions"];
    env.OPENCODE_CONFIG_CONTENT = JSON.stringify({
      ...config,
      ...(connection.defaultModel
        ? {
            model: `codespace/${connection.defaultModel}`,
            small_model: `codespace/${connection.defaultModel}`,
          }
        : {}),
      provider: {
        ...(config.provider as Record<string, unknown>),
        codespace: {
          name: "Codespace",
          npm,
          options: {
            baseURL: connection.baseUrl,
            apiKey: connection.apiKeyEnv
              ? `{env:${connection.apiKeyEnv}}`
              : "codespace-no-auth",
          },
          models: Object.fromEntries(
            connection.models.map((model) => [
              model.id,
              {
                name: model.name ?? model.id,
                limit: {
                  context: model.contextWindow,
                  output: model.maxTokens,
                },
              },
            ]),
          ),
        },
      },
    });
  }
  return env;
}

export function codespaceCodexArguments(
  backend: "codex" | "codex-oss",
): string[] {
  const connection = codespaceConnection(backend);
  if (!connection) return [];
  // One inline table replaces this provider's auth settings as well as its URL.
  // Values are TOML basic strings; JSON quoting handles their shared escapes.
  const fields = {
    name: "Codespace",
    base_url: connection.baseUrl,
    wire_api: "responses",
    requires_openai_auth: false,
    ...(connection.apiKeyEnv ? { env_key: connection.apiKeyEnv } : {}),
  };
  return [
    "-c",
    `model_providers.codespace={${Object.entries(fields)
      .map(([key, value]) => `${key}=${JSON.stringify(value)}`)
      .join(",")}}`,
    "-c",
    'model_provider="codespace"',
    ...(connection.defaultModel
      ? ["-c", `model=${JSON.stringify(connection.defaultModel)}`]
      : []),
  ];
}
