// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  deleteEndpointArguments,
  parseEndpointList,
  setEndpointArguments,
} from "../src/cli.js";

describe("Codespace endpoint CLI adapter", () => {
  it("parses the versioned endpoint list", () => {
    assert.deepEqual(
      parseEndpointList(
        '{"version":1,"endpoints":[{"port":3000,"label":"Application","visibility":"public"}]}',
      ),
      [{ port: 3000, label: "Application", visibility: "public" }],
    );
  });

  it("rejects malformed and duplicate endpoints", () => {
    assert.throws(() => parseEndpointList('{"version":2,"endpoints":[]}'));
    assert.throws(() =>
      parseEndpointList(
        '{"version":1,"endpoints":[{"port":3000,"label":"a","visibility":"public"},{"port":3000,"label":"b","visibility":"authenticated"}]}',
      ),
    );
  });

  it("constructs set and delete argument arrays", () => {
    assert.deepEqual(
      setEndpointArguments({
        port: 3000,
        label: "Web app",
        visibility: "public",
      }),
      ["endpoint", "set", "3000", "--label", "Web app", "--public"],
    );
    assert.deepEqual(deleteEndpointArguments(3000), [
      "endpoint",
      "delete",
      "3000",
    ]);
  });
});
