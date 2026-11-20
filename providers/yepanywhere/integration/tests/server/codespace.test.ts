import { describe, expect, it, vi } from "vitest";
import { createCodespaceRoutes } from "../../src/routes/codespace.js";

const state = {
  version: 1,
  sources: [{ name: "workstation" }],
  devices: [
    {
      source: "workstation",
      busId: "1-3",
      port: 0,
      state: "Port in Use",
      description: "Disk",
    },
  ],
};

function fixture() {
  const run = vi.fn(async (_file: string, args: string[]) =>
    JSON.stringify(
      args[0] === "status" ? state : { version: 1, endpoints: [] },
    ),
  );
  const available = vi.fn(async () => true);
  const app = createCodespaceRoutes({
    run,
    available,
    env: {
      CODESPACE_SSH_USER: "cs-1234",
      CODESPACE_SSH_HOST: "ssh.example.test",
      CODESPACE_SSH_PORT: "2222",
      TOKEN: "secret",
    },
  });
  return { app, run, available };
}

describe("Codespace CLI routes", () => {
  it("exposes only connection metadata and forwards validated arguments without a shell", async () => {
    const { app, run } = fixture();
    expect(await (await app.request("/capabilities")).json()).toEqual({
      ports: true,
      usb: true,
      ssh: { user: "cs-1234", host: "ssh.example.test", port: 2222 },
    });
    const response = await app.request("/ports", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        port: 8080,
        label: "$(touch /tmp/no)",
        visibility: "public",
      }),
    });
    expect(response.status).toBe(200);
    expect(run.mock.calls[0]?.slice(0, 2)).toEqual([
      "/usr/local/bin/gitea-codespace-endpoint",
      ["set", "8080", "--label", "$(touch /tmp/no)", "--public"],
    ]);
    expect((await app.request("/ports/0", { method: "DELETE" })).status).toBe(
      400,
    );
    run.mockResolvedValueOnce("invalid JSON");
    expect((await app.request("/ports")).status).toBe(502);
  });

  it("disconnects only the attachment the user confirmed and rejects unsupported USB", async () => {
    const { app, run, available } = fixture();
    const request = (busId: string) =>
      app.request("/usb/detach", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ port: 0, source: "workstation", busId }),
      });
    expect((await request("1-9")).status).toBe(409);
    expect((await request("1-3")).status).toBe(200);
    expect(run.mock.calls.at(-1)?.[1]).toEqual(["detach", "0"]);
    available.mockResolvedValue(false);
    expect((await app.request("/usb/status")).status).toBe(404);
  });
});
