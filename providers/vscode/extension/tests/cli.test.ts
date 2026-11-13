// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { parseEndpointList, RuntimeCommand } from "../src/cli.js";

describe("Codespace endpoint CLI adapter", () => {
  it("validates the versioned endpoint list and unique ports", () => {
    assert.deepEqual(
      parseEndpointList(
        '{"version":1,"endpoints":[{"port":3000,"label":"Application","visibility":"public"}]}',
      ),
      [{ port: 3000, label: "Application", visibility: "public" }],
    );
    assert.throws(() => parseEndpointList("null"), /unsupported response/);
    assert.throws(() => parseEndpointList('{"version":2,"endpoints":[]}'));
    assert.throws(() =>
      parseEndpointList(
        '{"version":1,"endpoints":[{"port":3000,"label":"a","visibility":"public"},{"port":3000,"label":"b","visibility":"authenticated"}]}',
      ),
    );
  });

  it("reports command stderr and retains the process failure", async () => {
    class Command extends RuntimeCommand {
      execute() {
        return this.run([
          "-e",
          "process.stderr.write('runtime unavailable'); process.exit(1)",
        ]);
      }
    }
    const messages: string[] = [];
    const command = new Command(process.execPath, 5000, (message) =>
      messages.push(message),
    );
    try {
      await assert.rejects(command.execute(), (error: Error) => {
        assert.match(error.message, /: runtime unavailable$/);
        assert.ok(error.cause instanceof Error);
        assert.equal(messages.at(-1), error.message);
        return true;
      });
    } finally {
      command.dispose();
    }
  });
});
