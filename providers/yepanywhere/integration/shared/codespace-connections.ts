// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

export const CODESPACE_CONNECTION_BACKENDS = [
  "codex",
  "codex-oss",
  "claude",
  "opencode",
  "pi",
] as const;
export type CodespaceBackend = (typeof CODESPACE_CONNECTION_BACKENDS)[number];
const connectionAPIs = [
  "openai-completions",
  "openai-responses",
  "anthropic-messages",
] as const;
export interface CodespaceModel {
  id: string;
  name?: string;
  contextWindow?: number;
  maxTokens?: number;
}
export interface CodespaceConnection {
  baseUrl: string;
  apiKeyEnv?: string;
  models: CodespaceModel[];
  defaultModel?: string;
  api?: (typeof connectionAPIs)[number];
  authMode?: "api-key" | "bearer";
}
export type CodespaceConnections = Partial<
  Record<CodespaceBackend, CodespaceConnection | null>
>;

const modelSchema = {
  type: "object",
  properties: {
    id: { type: "string", minLength: 1, pattern: "^[^\\s]+$" },
    name: { type: "string", minLength: 1 },
    contextWindow: { type: "integer", minimum: 1 },
  },
  required: ["id"],
  additionalProperties: false,
};

const connectionSchemas = Object.fromEntries(
  CODESPACE_CONNECTION_BACKENDS.map(
    (backend) =>
      [
        backend,
        {
          type: "object",
          properties: {
            baseUrl: { type: "string", pattern: "^https?://[^\\s]+$" },
            apiKeyEnv: {
              type: "string",
              pattern: "^[A-Za-z_][A-Za-z0-9_]*$",
            },
            models: {
              type: "array",
              items: {
                ...modelSchema,
                properties: {
                  ...modelSchema.properties,
                  ...(backend === "opencode" || backend === "pi"
                    ? { maxTokens: { type: "integer", minimum: 1 } }
                    : {}),
                },
              },
            },
            defaultModel: { type: "string", minLength: 1 },
            ...(backend === "claude"
              ? { authMode: { type: "string", enum: ["api-key", "bearer"] } }
              : {}),
            ...(backend === "opencode" || backend === "pi"
              ? {
                  api: {
                    type: "string",
                    enum: connectionAPIs,
                  },
                }
              : {}),
          },
          required: [
            "baseUrl",
            ...(backend === "opencode" || backend === "pi" ? ["api"] : []),
          ],
          additionalProperties: false,
        },
      ] as const,
  ),
);

export const codespaceConnectionsSchema = {
  type: "object",
  properties: Object.fromEntries(
    Object.entries(connectionSchemas).map(([backend, schema]) => [
      backend,
      { anyOf: [{ type: "null" }, schema] },
    ]),
  ),
  additionalProperties: false,
};

// Validate at prepare and launch so saved configuration has the same contract
// as template and repository input. Errors name fields, never their values.
export function parseCodespaceConnections(
  value: unknown,
  path = "connections",
): CodespaceConnections {
  if (value === undefined) return {};
  const fail = (field: string, reason: string): never => {
    throw new Error(`${path}${field}: ${reason}`);
  };
  if (!value || typeof value !== "object" || Array.isArray(value))
    return fail("", "must be an object");
  const result: CodespaceConnections = {};
  for (const [key, raw] of Object.entries(value)) {
    const schema = connectionSchemas[key];
    if (!schema || !Object.hasOwn(connectionSchemas, key))
      return fail(`.${key}`, "unsupported backend");
    const backend = key as CodespaceBackend;
    if (raw === null) {
      result[backend] = null;
      continue;
    }
    if (!raw || typeof raw !== "object" || Array.isArray(raw))
      fail(`.${key}`, "must be an object or null");
    const connection = raw as CodespaceConnection;
    for (const field of Object.keys(connection))
      if (!Object.hasOwn(schema.properties, field))
        fail(`.${key}.${field}`, "unsupported field");
    try {
      if (
        typeof connection.baseUrl !== "string" ||
        /\s/.test(connection.baseUrl)
      )
        throw new Error();
      const url = new URL(connection.baseUrl);
      if (
        !["http:", "https:"].includes(url.protocol) ||
        !url.hostname ||
        url.username ||
        url.password ||
        url.search ||
        url.hash
      )
        throw new Error();
    } catch {
      fail(
        `.${key}.baseUrl`,
        "must be an HTTP(S) API base URL without credentials, query or fragment",
      );
    }
    if (
      connection.apiKeyEnv !== undefined &&
      (typeof connection.apiKeyEnv !== "string" ||
        !/^[A-Za-z_][A-Za-z0-9_]*$/.test(connection.apiKeyEnv))
    )
      fail(`.${key}.apiKeyEnv`, "must be an environment variable name");
    if (
      backend === "claude" &&
      connection.authMode !== undefined &&
      !["api-key", "bearer"].includes(connection.authMode)
    )
      fail(`.${key}.authMode`, "must be api-key or bearer");
    if (
      (backend === "opencode" || backend === "pi") &&
      !connectionAPIs.some((api) => api === connection.api)
    )
      fail(`.${key}.api`, "must select a supported API protocol");
    if (connection.models !== undefined && !Array.isArray(connection.models))
      fail(`.${key}.models`, "must be an array");
    const ids = new Set<string>();
    const models = (connection.models ?? []).map((model, index) => {
      const field = `.${key}.models[${index}]`;
      if (!model || typeof model !== "object" || Array.isArray(model))
        fail(field, "must be an object");
      for (const name of Object.keys(model))
        if (!Object.hasOwn(schema.properties.models.items.properties, name))
          fail(`${field}.${name}`, "unsupported field");
      if (
        typeof model.id !== "string" ||
        !model.id ||
        /\s/.test(model.id) ||
        ids.has(model.id)
      )
        fail(
          `${field}.id`,
          "must be a unique non-empty model ID without whitespace",
        );
      ids.add(model.id);
      if (
        model.name !== undefined &&
        (typeof model.name !== "string" || !model.name.trim())
      )
        fail(`${field}.name`, "must be a non-empty name");
      for (const name of ["contextWindow", "maxTokens"] as const)
        if (
          model[name] !== undefined &&
          (!Number.isSafeInteger(model[name]) || model[name] <= 0)
        )
          fail(`${field}.${name}`, "must be a positive integer");
      // Resolve native model limits once so OpenCode and Pi use identical values.
      if (backend === "opencode" || backend === "pi") {
        const contextWindow = model.contextWindow ?? 128000;
        const maxTokens = model.maxTokens ?? Math.min(16384, contextWindow);
        if (maxTokens > contextWindow)
          fail(field, "maxTokens must not exceed contextWindow");
        return { ...model, contextWindow, maxTokens };
      }
      return { ...model };
    });
    if (
      connection.defaultModel !== undefined &&
      !ids.has(connection.defaultModel)
    )
      fail(`.${key}.defaultModel`, "must name a configured model");
    result[backend] = {
      ...connection,
      models,
      defaultModel: connection.defaultModel ?? models[0]?.id,
    };
  }
  return result;
}
