// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT
// biome-ignore-all lint/complexity/useLiteralKeys: Exercise provider launch boundaries without starting external model services.
import { parseCodespaceConnections } from "@yep-anywhere/shared";
import { afterEach, expect, it, vi } from "vitest";
import {
  codespaceAuthStatus,
  codespaceCodexArguments,
  codespaceEnvironment,
  codespaceModel,
} from "../../src/sdk/codespace-connections.js";
import { ClaudeProvider } from "../../src/sdk/providers/claude.js";
import { CodexProvider } from "../../src/sdk/providers/codex.js";
import { CodexOSSProvider } from "../../src/sdk/providers/codex-oss.js";
import { OpenCodeProvider } from "../../src/sdk/providers/opencode.js";
import { PiProvider } from "../../src/sdk/providers/pi.js";

const connection = {
  baseUrl: "https://api.example.test/custom/v1",
  apiKeyEnv: "COMPANY_TOKEN",
  models: [
    {
      id: "company/model",
      name: "Company model",
      contextWindow: 32768,
    },
  ],
};

afterEach(() => vi.unstubAllEnvs());

it("validates connection fields, model defaults and token limits", () => {
  expect(
    parseCodespaceConnections({ codex: connection, claude: null }),
  ).toEqual({
    codex: { ...connection, defaultModel: "company/model" },
    claude: null,
  });
  for (const [field, invalid] of [
    ["baseUrl", "https://user:private-secret@example.test/v1"],
    ["apiKeyEnv", "literal secret"],
    ["defaultModel", "other"],
    ["models", [{ id: "x" }, { id: "x" }]],
    ["api", "openai-completions"],
  ]) {
    expect(() =>
      parseCodespaceConnections({
        codex: { ...connection, [field as string]: invalid },
      }),
    ).toThrow(`connections.codex.${field}`);
  }
  expect(() => parseCodespaceConnections({ pi: connection })).toThrow(
    "connections.pi.api",
  );
  expect(() =>
    parseCodespaceConnections({ claude: { ...connection, authMode: "oauth" } }),
  ).toThrow("connections.claude.authMode");
  for (const backend of ["opencode", "pi"] as const) {
    const input = { ...connection, api: "openai-responses" };
    expect(
      parseCodespaceConnections({ [backend]: input })[backend]?.models,
    ).toEqual([{ ...connection.models[0], maxTokens: 16384 }]);
    expect(
      parseCodespaceConnections({
        [backend]: { ...input, models: [{ id: "small", contextWindow: 4096 }] },
      })[backend]?.models,
    ).toEqual([{ id: "small", contextWindow: 4096, maxTokens: 4096 }]);
    expect(() =>
      parseCodespaceConnections({
        [backend]: {
          ...input,
          models: [{ id: "oversized", maxTokens: 256000 }],
        },
      }),
    ).toThrow("maxTokens must not exceed contextWindow");
  }
});

it("uses optional connection catalogs for all five backends without discovery or API requests", async () => {
  const providers = [
    new CodexProvider(),
    new CodexOSSProvider(),
    new ClaudeProvider(),
    new OpenCodeProvider(),
    new PiProvider(),
  ];
  const connections = Object.fromEntries(
    providers.map(({ name }) => [
      name,
      {
        ...connection,
        ...(name === "opencode" || name === "pi"
          ? { api: "openai-completions" }
          : {}),
      },
    ]),
  );
  vi.stubEnv("CODESPACE_YEP_CONNECTIONS", JSON.stringify(connections));
  for (const provider of providers) {
    const id = ["pi", "opencode"].includes(provider.name)
      ? "codespace/company/model"
      : "company/model";
    expect(await provider.getAvailableModels()).toEqual([
      { id, name: "Company model", contextWindow: 32768, isDefault: true },
    ]);
    expect(codespaceModel(provider.name)).toBe(id);
    expect(codespaceModel(provider.name, id)).toBe(id);
    expect(() => codespaceModel(provider.name, "other/model")).toThrow(
      "selected model is not in the configured catalog",
    );
    expect(codespaceAuthStatus(provider.name, true)?.enabled).toBe(false);
    expect(() => codespaceEnvironment(provider.name, {})).toThrow(
      "environment variable COMPANY_TOKEN is missing or empty",
    );
  }
  vi.stubEnv("COMPANY_TOKEN", "rotated-secret");
  for (const provider of providers) {
    const { name } = provider;
    const requiresRegisteredModel = name === "opencode" || name === "pi";
    const input = {
      [name]: {
        ...connections[name],
        models: requiresRegisteredModel ? [] : undefined,
      },
    };
    vi.stubEnv("CODESPACE_YEP_CONNECTIONS", JSON.stringify(input));
    expect(await provider.getAvailableModels()).toEqual([]);
    if (requiresRegisteredModel) {
      expect(() => codespaceModel(name)).toThrow(
        "select a codespace model before starting a session",
      );
    } else {
      expect(codespaceModel(name)).toBeUndefined();
      expect(codespaceModel(name, "default")).toBeUndefined();
    }
    const selected = requiresRegisteredModel ? "codespace/custom" : "custom";
    expect(codespaceModel(name, selected)).toBe(selected);
    expect(codespaceAuthStatus(name, true)?.enabled).toBe(true);
    const env = codespaceEnvironment(name, {
      CODESPACE_YEP_CONNECTIONS: "{}",
      COMPANY_TOKEN: "old",
      HOME: "/home/user",
    });
    expect(JSON.parse(env.CODESPACE_YEP_CONNECTIONS ?? "{}")).toEqual(
      parseCodespaceConnections(input),
    );
    expect(env.COMPANY_TOKEN).toBe("rotated-secret");
    expect(env.HOME).toBe("/home/user");
  }
});

it("keeps Codex exec new and resumed turns on the same scoped Responses connection", () => {
  vi.stubEnv(
    "CODESPACE_YEP_CONNECTIONS",
    JSON.stringify({
      codex: { ...connection, models: [] },
      "codex-oss": { ...connection, models: [] },
    }),
  );
  const args = codespaceCodexArguments("codex");
  expect(args.join(" ")).toContain(
    'base_url="https://api.example.test/custom/v1"',
  );
  expect(args.join(" ")).toContain('env_key="COMPANY_TOKEN"');
  expect(args.join(" ")).toContain("requires_openai_auth=false");
  expect(args.some((arg) => arg.startsWith("model="))).toBe(false);
  const provider = new CodexOSSProvider();
  const options = { cwd: "/workspace", model: "company/model" };
  const route = { serviceId: undefined, modelId: "company/model" };
  expect(provider["buildFirstTurnArgs"](options, route)).toEqual(
    expect.arrayContaining(args),
  );
  expect(
    provider["buildResumeTurnArgs"](options, route, "session", "prompt"),
  ).toEqual(expect.arrayContaining(args));
});

it("scopes Claude SDK settings and OpenCode native configuration while preserving user storage", () => {
  vi.stubEnv("COMPANY_TOKEN", "example-token");
  vi.stubEnv(
    "CODESPACE_YEP_CONNECTIONS",
    JSON.stringify({
      claude: { ...connection, authMode: "bearer" },
      opencode: { ...connection, api: "openai-responses" },
    }),
  );
  const claude = new ClaudeProvider();
  const env = claude["getSettings"]()?.env;
  expect(env).toMatchObject({
    ANTHROPIC_BASE_URL: connection.baseUrl,
    ANTHROPIC_AUTH_TOKEN: "example-token",
    ANTHROPIC_API_KEY: "",
    CLAUDE_CODE_OAUTH_TOKEN: "",
  });
  expect(claude["getSettings"]()?.apiKeyHelper).toBe("");
  const opencode = codespaceEnvironment("opencode", {
    HOME: "/home/user",
    PATH: "/feature/bin",
    OPENCODE_CONFIG_CONTENT: '{"theme":"system","provider":{"local":{}}}',
  });
  expect(opencode.HOME).toBe("/home/user");
  expect(opencode.PATH).toBe("/feature/bin");
  expect(opencode.OPENCODE_CONFIG_CONTENT).not.toContain("example-token");
  expect(JSON.parse(opencode.OPENCODE_CONFIG_CONTENT ?? "{}")).toMatchObject({
    theme: "system",
    model: "codespace/company/model",
    provider: {
      local: {},
      codespace: {
        npm: "@ai-sdk/openai",
        options: { baseURL: connection.baseUrl, apiKey: "{env:COMPANY_TOKEN}" },
      },
    },
  });
  vi.stubEnv(
    "CODESPACE_YEP_CONNECTIONS",
    JSON.stringify({
      claude: { baseUrl: connection.baseUrl, models: connection.models },
      codex: null,
    }),
  );
  expect(
    codespaceEnvironment("claude", { ANTHROPIC_AUTH_TOKEN: "unrelated-token" }),
  ).toMatchObject({
    ANTHROPIC_API_KEY: "codespace-no-auth",
    ANTHROPIC_AUTH_TOKEN: "",
  });
  const nativeEnv = { OPENAI_API_KEY: "native-token" };
  expect(codespaceEnvironment("codex", nativeEnv)).toBe(nativeEnv);
});
