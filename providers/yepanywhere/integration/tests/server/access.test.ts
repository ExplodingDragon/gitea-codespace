import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, it } from "vitest";
import { AuthService } from "../../src/auth/AuthService.js";
import { LimitedUsersService } from "../../src/auth/LimitedUsersService.js";
import { SESSION_COOKIE_NAME } from "../../src/auth/routes.js";
import { RemoteAccessService } from "../../src/remote-access/RemoteAccessService.js";
import { deriveWsConnectionPolicy } from "../../src/routes/ws-auth-policy.js";
import { ServerSettingsService } from "../../src/services/ServerSettingsService.js";
import { createApp } from "../setup/create-app.js";

it("uses Gateway identity and local WebSocket access after restoring app settings", async () => {
  const dataDir = await mkdtemp(join(tmpdir(), "codespace-access-"));
  const settings = new ServerSettingsService({ dataDir });
  const remote = new RemoteAccessService({ dataDir });
  const auth = new AuthService({ dataDir, cookieSecret: "test-secret" });
  const users = new LimitedUsersService({ dataDir });
  let instance: ReturnType<typeof createApp> | undefined;
  try {
    await writeFile(
      join(dataDir, "remote-access.json"),
      JSON.stringify({
        version: 1,
        enabled: true,
        credentials: { salt: "saved-salt", verifier: "saved-verifier" },
        relay: { url: "wss://relay.invalid/ws", username: "saved-owner" },
      }),
    );
    await writeFile(
      join(dataDir, "server-settings.json"),
      JSON.stringify({
        version: 2,
        settings: {
          publicSharesEnabled: true,
          limitedUsersEnabled: true,
          codexReasoningSummary: "detailed",
        },
      }),
    );
    await settings.initialize();
    await remote.initialize();
    await auth.initialize();
    await users.initialize();
    await auth.enableAuth("saved-password");
    const cookie = await auth.createSession("test-browser", "saved-user");
    expect(remote.isEnabled()).toBe(false);
    expect(remote.getRelayConfig()).toBeNull();
    expect(
      deriveWsConnectionPolicy({
        remoteAccessEnabled: remote.isEnabled(),
        hasSessionCookieAuth: false,
        isRelayConnection: false,
        isLoopbackConnection: false,
      }),
    ).toBe("local_unrestricted");
    expect(settings.getSetting("publicSharesEnabled")).toBe(false);
    expect(settings.getSetting("limitedUsersEnabled")).toBe(false);
    const changed = await settings.updateSettings({
      publicSharesEnabled: true,
      limitedUsersEnabled: true,
    });
    expect(changed).toMatchObject({
      publicSharesEnabled: false,
      limitedUsersEnabled: false,
      codexReasoningSummary: "detailed",
    });
    instance = createApp({
      dataDir,
      authDisabled: true,
      authService: auth,
      limitedUsersService: users,
      remoteAccessService: remote,
      serverSettingsService: settings,
    });
    const headers = {
      "X-Yep-Anywhere": "true",
      Cookie: `${SESSION_COOKIE_NAME}=${cookie}`,
    };
    const status = await instance.app.request("/api/auth/status", { headers });
    expect(await status.json()).toMatchObject({
      authenticated: true,
      disabledByEnv: true,
    });
    const principal = await instance.app.request("/api/users/me", { headers });
    expect(await principal.json()).toMatchObject({
      superuser: true,
      username: null,
      enabled: false,
      hasLimitedUsers: false,
    });
    for (const url of [
      "/api/auth/enable",
      "/api/users/switch",
      "/api/remote-access/enable",
      "/api/remote-access/relay",
    ]) {
      const response = await instance.app.request(url, {
        method: "POST",
        headers,
      });
      expect(response.status, url).toBe(403);
      expect(await response.json()).toEqual({
        error: "Access is managed by Codespace Gateway",
      });
    }
    expect(
      (await instance.app.request("/public-api/shares/sessions/saved")).status,
    ).toBe(403);
  } finally {
    await instance?.disposeSessionReaders();
    await rm(dataDir, { recursive: true, force: true });
  }
});
