// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { it } from "node:test";
import { parseUSBResponse } from "../src/usb-client.js";

it("reads imported devices independently of forwarding, including VHCI port zero", () => {
  const device = {
    source: "",
    busId: "1-1",
    description: "CDC ACM",
    port: 0,
    state: "Port in Use",
  };
  assert.deepEqual(
    parseUSBResponse(
      JSON.stringify({ version: 1, sources: [], devices: [device] }),
      true,
    ),
    {
      sources: [],
      devices: [device],
    },
  );
  assert.throws(() =>
    parseUSBResponse(
      '{"version":1,"sources":[{"name":"lab"}],"devices":[{"source":"lab","busId":"1-1","description":"CDC ACM"}]}',
      true,
    ),
  );
  assert.throws(() => parseUSBResponse('{"version":2,"devices":[]}', false));
  for (const name of [123, null, undefined]) {
    assert.throws(() =>
      parseUSBResponse(
        JSON.stringify({ version: 1, sources: [{ name }], devices: [] }),
        false,
      ),
    );
  }
});

it("activates capability discovery and gates every USB entry point", async () => {
  const manifest = JSON.parse(
    await readFile(resolve(__dirname, "../package.json"), "utf8"),
  );
  assert.ok(manifest.activationEvents.includes("onStartupFinished"));
  for (const command of manifest.contributes.commands.filter(
    (item: { command: string }) =>
      item.command.startsWith("giteaCodespace.usb."),
  )) {
    assert.match(command.enablement, /giteaCodespace.usbAvailable/);
    assert.equal(
      manifest.contributes.menus.commandPalette.find(
        (item: { command: string }) => item.command === command.command,
      ).when,
      "giteaCodespace.usbAvailable",
    );
  }
  assert.equal(
    manifest.contributes.views.explorer.find(
      (item: { id: string }) => item.id === "giteaCodespace.usb",
    ).when,
    "giteaCodespace.usbAvailable",
  );
});
