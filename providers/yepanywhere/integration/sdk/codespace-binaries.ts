import { accessSync, constants } from "node:fs";
import { join } from "node:path";

/** External discovery runs first; the image supplies only the final fallback. */
export function bundledCodespaceBinary(
  name: "codex" | "opencode" | "pi",
): string | null {
  const root = process.env.CODESPACE_WEB_IDE_PROGRAM_DIR;
  if (!root) return null;
  const binary = join(root, "program", "tools", "bin", name);
  try {
    accessSync(binary, constants.X_OK);
    return binary;
  } catch {
    return null;
  }
}
