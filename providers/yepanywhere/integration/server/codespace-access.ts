import type { MiddlewareHandler } from "hono";
import {
  DIRECT_LOGIN_VARIABLE,
  PRINCIPAL_VARIABLE,
  SUPERUSER,
} from "../auth/principal.js";

// Gateway owns browser access. Keep the upstream status reads used by the UI,
// while refusing configuration of a second authentication or remote entry point.
export const codespaceAccess: MiddlewareHandler = async (c, next) => {
  const path = c.req.path.replace(/\/+$/, "");
  if (/^\/api\/(auth|users|remote-access)(\/|$)/.test(path)) {
    const status = [
      "/api/auth/status",
      "/api/users/me",
      "/api/remote-access/config",
      "/api/remote-access/relay",
      "/api/remote-access/relay/status",
    ];
    if (c.req.method !== "GET" || !status.includes(path)) {
      return c.json({ error: "Access is managed by Codespace Gateway" }, 403);
    }
  }
  if (path.startsWith("/public-api/")) {
    return c.json(
      { error: "Open this workspace through Codespace Gateway" },
      403,
    );
  }
  c.set(PRINCIPAL_VARIABLE, SUPERUSER);
  c.set(DIRECT_LOGIN_VARIABLE, null);
  await next();
};
