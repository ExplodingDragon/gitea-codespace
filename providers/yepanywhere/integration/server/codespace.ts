import { execFile } from "node:child_process";
import { constants } from "node:fs";
import { access } from "node:fs/promises";
import { promisify } from "node:util";
import type { CodespaceCapabilities } from "@yep-anywhere/shared";
import { Hono } from "hono";
import { HTTPException } from "hono/http-exception";
import { z } from "zod";
import { PRINCIPAL_VARIABLE, type Principal } from "../auth/principal.js";

const executables = {
  ports: "/usr/local/bin/gitea-codespace-endpoint",
  usb: "/usr/local/bin/gitea-codespace-usbip",
};
const exec = promisify(execFile);
const port = z.number().int().min(1).max(65535);
const vhciPort = z.number().int().min(0).max(65535);
const source = z.string().regex(/^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$/);
const busId = z.string().regex(/^[0-9]+-[0-9]+(?:\.[0-9]+)*$/);
const endpoint = z
  .object({
    port,
    label: z
      .string()
      .trim()
      .min(1)
      .refine((value) => Buffer.byteLength(value) <= 128),
    visibility: z.enum(["authenticated", "public"]),
  })
  .strict();
const device = z.object({
  source: source.or(z.literal("")),
  busId: z.string(),
  description: z.string(),
});
const usbList = z.object({
  version: z.literal(1),
  sources: z.array(z.object({ name: source, error: z.string().optional() })),
  devices: z.array(device.extend({ source, busId })),
});
const usbStatus = usbList.extend({
  devices: z.array(device.extend({ port: vhciPort, state: z.string() })),
});
const endpoints = z.object({
  version: z.literal(1),
  endpoints: z.array(endpoint),
});

async function available(file: string): Promise<boolean> {
  try {
    await access(file, constants.X_OK);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
    throw error;
  }
}

async function run(
  command: string,
  args: string[],
  signal: AbortSignal,
): Promise<string> {
  try {
    const result = await exec(command, args, {
      encoding: "utf8",
      maxBuffer: 1024 * 1024,
      timeout: command === executables.usb ? 35_000 : 5_000,
      signal,
    });
    return result.stdout;
  } catch (error) {
    const failure = error as Error & { stderr?: string };
    throw new HTTPException(502, {
      message: failure.stderr?.trim() || failure.message,
    });
  }
}

// Dependencies keep the CLI boundary testable without a running Codespace.
export function createCodespaceRoutes(
  deps = { available, run, env: process.env },
) {
  async function readCLI<T>(
    tool: keyof typeof executables,
    operation: "list" | "status",
    schema: z.ZodType<T>,
    signal: AbortSignal,
  ): Promise<T> {
    const output = await deps.run(
      executables[tool],
      [operation, "--format", "json"],
      signal,
    );
    try {
      return schema.parse(JSON.parse(output));
    } catch {
      throw new HTTPException(502, {
        message: "Invalid Codespace CLI response",
      });
    }
  }

  const app = new Hono<{ Variables: { yaPrincipal: Principal } }>();
  app.use("*", async (c, next) => {
    c.header("Cache-Control", "no-store");
    if (c.get(PRINCIPAL_VARIABLE)?.kind !== "superuser")
      return c.json({ error: "Forbidden" }, 403);
    await next();
  });
  app.onError((error, c) => {
    if (error instanceof z.ZodError || error instanceof SyntaxError)
      return c.json({ error: "Invalid Codespace request" }, 400);
    return c.json(
      { error: error.message },
      error instanceof HTTPException ? error.status : 500,
    );
  });
  app.get("/capabilities", async (c) => {
    const result: CodespaceCapabilities = {
      ports: await deps.available(executables.ports),
      usb: await deps.available(executables.usb),
    };
    const {
      CODESPACE_SSH_USER: user,
      CODESPACE_SSH_HOST: host,
      CODESPACE_SSH_PORT: rawPort,
    } = deps.env;
    const sshPort = Number(rawPort);
    if (
      user &&
      /^cs-[a-z0-9-]+$/.test(user) &&
      host &&
      /^[a-zA-Z0-9.[\]:-]+$/.test(host) &&
      !host.startsWith("-") &&
      port.safeParse(sshPort).success
    )
      result.ssh = { user, host, port: sshPort };
    return c.json(result);
  });
  app.get("/ports", async (c) =>
    c.json(await readCLI("ports", "list", endpoints, c.req.raw.signal)),
  );
  app.post("/ports", async (c) => {
    const value = endpoint.parse(await c.req.json());
    await deps.run(
      executables.ports,
      [
        "set",
        String(value.port),
        "--label",
        value.label,
        ...(value.visibility === "public" ? ["--public"] : []),
      ],
      c.req.raw.signal,
    );
    return c.json({ ok: true });
  });
  app.delete("/ports/:port", async (c) => {
    const value = port.parse(Number(c.req.param("port")));
    await deps.run(
      executables.ports,
      ["delete", String(value)],
      c.req.raw.signal,
    );
    return c.json({ ok: true });
  });
  app.use("/usb/*", async (c, next) => {
    if (!(await deps.available(executables.usb)))
      return c.json({ error: "USB is unavailable" }, 404);
    await next();
  });
  for (const command of ["status", "list"] as const) {
    app.get(`/usb/${command}`, async (c) =>
      c.json(
        await readCLI(
          "usb",
          command,
          command === "status" ? usbStatus : usbList,
          c.req.raw.signal,
        ),
      ),
    );
  }
  app.post("/usb/attach", async (c) => {
    const value = z
      .object({ source, busId })
      .strict()
      .parse(await c.req.json());
    await deps.run(
      executables.usb,
      ["attach", value.busId, "--source", value.source],
      c.req.raw.signal,
    );
    return c.json({ ok: true });
  });
  app.post("/usb/detach", async (c) => {
    const value = z
      .object({
        port: vhciPort,
        source: source.or(z.literal("")),
        busId: z.string(),
      })
      .strict()
      .parse(await c.req.json());
    const state = await readCLI("usb", "status", usbStatus, c.req.raw.signal);
    if (
      !state.devices.some(
        (device) =>
          device.port === value.port &&
          device.source === value.source &&
          device.busId === value.busId,
      )
    ) {
      return c.json(
        { error: "USB attachment changed; refresh before disconnecting" },
        409,
      );
    }
    await deps.run(
      executables.usb,
      ["detach", String(value.port)],
      c.req.raw.signal,
    );
    return c.json({ ok: true });
  });
  return app;
}
