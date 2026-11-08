// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const endpointCLI = "/usr/local/bin/gitea-codespace-endpoint";

export type EndpointVisibility = "authenticated" | "public";

export interface Endpoint {
  port: number;
  label: string;
  visibility: EndpointVisibility;
}

interface EndpointList {
  version: number;
  endpoints: unknown;
}

export function parseEndpointList(output: string): Endpoint[] {
  let document: EndpointList;
  try {
    document = JSON.parse(output) as EndpointList;
  } catch {
    throw new Error("The Codespace endpoint command returned invalid JSON");
  }
  if (document.version !== 1 || !Array.isArray(document.endpoints)) {
    throw new Error(
      "The Codespace endpoint command returned an unsupported response",
    );
  }
  const ports = new Set<number>();
  return document.endpoints.map((value) => {
    if (!value || typeof value !== "object") {
      throw new Error(
        "The Codespace endpoint command returned an invalid endpoint",
      );
    }
    const endpoint = value as Partial<Endpoint>;
    const port = endpoint.port;
    if (
      typeof port !== "number" ||
      !Number.isInteger(port) ||
      port < 1 ||
      port > 65535 ||
      typeof endpoint.label !== "string" ||
      (endpoint.visibility !== "authenticated" &&
        endpoint.visibility !== "public")
    ) {
      throw new Error(
        "The Codespace endpoint command returned an invalid endpoint",
      );
    }
    if (ports.has(port)) {
      throw new Error(
        `The Codespace endpoint command returned duplicate port ${port}`,
      );
    }
    ports.add(port);
    return endpoint as Endpoint;
  });
}

export function setEndpointArguments(endpoint: Endpoint): string[] {
  const arguments_ = [
    "endpoint",
    "set",
    String(endpoint.port),
    "--label",
    endpoint.label,
  ];
  if (endpoint.visibility === "public") arguments_.push("--public");
  return arguments_;
}

export function deleteEndpointArguments(port: number): string[] {
  return ["endpoint", "delete", String(port)];
}

export class EndpointClient {
  private readonly abortController = new AbortController();

  constructor(private readonly log: (message: string) => void) {}

  dispose(): void {
    this.abortController.abort();
  }

  async list(): Promise<Endpoint[]> {
    const { stdout } = await this.run(["endpoint", "list", "--format", "json"]);
    return parseEndpointList(stdout);
  }

  async set(endpoint: Endpoint): Promise<void> {
    await this.run(setEndpointArguments(endpoint));
  }

  async delete(port: number): Promise<void> {
    await this.run(deleteEndpointArguments(port));
  }

  private async run(
    arguments_: string[],
  ): Promise<{ stdout: string; stderr: string }> {
    this.log(`$ ${endpointCLI} ${arguments_.map(shellQuote).join(" ")}`);
    try {
      const result = await execFileAsync(endpointCLI, arguments_, {
        encoding: "utf8",
        signal: this.abortController.signal,
        timeout: 5000,
        maxBuffer: 1024 * 1024,
      });
      if (result.stderr.trim()) this.log(result.stderr.trimEnd());
      return result;
    } catch (error) {
      const detail = error instanceof Error ? error.message : String(error);
      this.log(detail);
      throw error;
    }
  }
}

function shellQuote(value: string): string {
  return /^[A-Za-z0-9_./:@+-]+$/.test(value)
    ? value
    : `'${value.replaceAll("'", "'\\''")}'`;
}
