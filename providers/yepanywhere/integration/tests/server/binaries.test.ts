// biome-ignore-all lint/complexity/useLiteralKeys: Verify executable and backend selection without starting model services.
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, expect, it, vi } from "vitest";
import { findCodexCliPath } from "../../src/sdk/cli-detection.js";
import { setCliInstallationService } from "../../src/sdk/cli-installation-context.js";
import { configureCliInstallations } from "../../src/sdk/cli-installation-runtime.js";
import { OpenCodeProvider } from "../../src/sdk/providers/opencode.js";
import { PiProvider } from "../../src/sdk/providers/pi.js";
import { getAllProviders } from "../../src/sdk/providers/index.js";
import { createApp } from "../setup/create-app.js";

vi.mock("node:os", async (original) => ({
  ...(await original<typeof import("node:os")>()),
  homedir: () => process.env.HOME,
}));

afterEach(() => vi.unstubAllEnvs());

it("selects explicit and external programs before image fallbacks", async () => {
  const root = await mkdtemp(join(tmpdir(), "codespace-binaries-"));
  try {
    const bin = join(root, "program/tools/bin");
    const external = join(root, "external");
    await mkdir(bin, { recursive: true });
    await mkdir(external);
    vi.stubEnv("CODESPACE_WEB_IDE_PROGRAM_DIR", root);
    vi.stubEnv("HOME", root);
    vi.stubEnv("PATH", external);
    vi.stubEnv("PI_EXECUTABLE", "");
    vi.stubEnv("PI_PATH", "");
    configureCliInstallations(
      root,
      () => ({}),
      () => ({}),
    );
    await writeFile(join(external, "which"), "#!/bin/sh\nexit 1\n", {
      mode: 0o755,
    });
    for (const name of ["codex", "opencode", "pi"]) {
      await writeFile(join(bin, name), "#!/bin/sh\necho codex-cli 9.0.0\n", {
        mode: 0o755,
      });
    }
    const opencode = new OpenCodeProvider();
    const pi = new PiProvider();
    expect(await findCodexCliPath()).toBe(join(bin, "codex"));
    expect(await opencode["findOpenCodePath"]()).toBe(join(bin, "opencode"));
    expect((await pi["findPiLaunchTarget"]())?.command).toBe(join(bin, "pi"));

    for (const name of ["codex", "opencode", "pi"]) {
      await writeFile(
        join(external, name),
        "#!/bin/sh\necho codex-cli 0.167.0\n",
        { mode: 0o755 },
      );
    }
    await writeFile(
      join(external, "which"),
      '#!/bin/sh\nfor name do :; done\nprintf "%s/%s\\n" "$PATH" "$name"\n',
    );
    expect(await findCodexCliPath()).toBe(join(external, "codex"));
    expect(await opencode["findOpenCodePath"]()).toBe(
      join(external, "opencode"),
    );
    expect((await pi["findPiLaunchTarget"]())?.command).toBe(
      join(external, "pi"),
    );
    expect(await findCodexCliPath(join(bin, "codex"))).toBe(join(bin, "codex"));
    expect(
      await new OpenCodeProvider({ opencodePath: join(bin, "opencode") })[
        "findOpenCodePath"
      ](),
    ).toBe(join(bin, "opencode"));
    expect(
      (
        await new PiProvider({ piPath: join(bin, "pi") })[
          "findPiLaunchTarget"
        ]()
      )?.command,
    ).toBe(join(bin, "pi"));
    const missing = join(root, "missing");
    expect(await findCodexCliPath(missing)).toBeNull();
    expect(
      await new OpenCodeProvider({ opencodePath: missing }).isInstalled(),
    ).toBe(false);
    expect(await new PiProvider({ piPath: missing }).isInstalled()).toBe(false);
  } finally {
    setCliInstallationService(undefined);
    await rm(root, { recursive: true, force: true });
  }
});

it("starts with Codex as the only configured backend", async () => {
  vi.stubEnv("ENABLED_PROVIDERS", "codex");
  expect(getAllProviders().map((provider) => provider.name)).toEqual(["codex"]);
  // Override the test fixture's null provider to exercise production startup.
  const instance = createApp({ provider: undefined });
  try {
    expect(instance.supervisor["resolveProvider"]()?.name).toBe("codex");
  } finally {
    await instance.disposeSessionReaders();
  }
});
