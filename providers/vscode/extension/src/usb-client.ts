// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

import { RuntimeCommand } from "./cli.js";

export interface USBDevice {
  source: string;
  busId: string;
  description: string;
}

export interface USBImportedDevice extends USBDevice {
  port: number;
  state: string;
}

export interface USBList {
  sources: { name: string; error?: string }[];
  devices: USBDevice[];
}

export interface USBStatus extends USBList {
  devices: USBImportedDevice[];
}

const busIDPattern = /^[0-9]+-[0-9]+(?:\.[0-9]+)*$/;
const sourcePattern = /^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$/;

export function parseUSBResponse(output: string, imported: true): USBStatus;
export function parseUSBResponse(output: string, imported: false): USBList;
export function parseUSBResponse(output: string, imported: boolean): USBList {
  const document = JSON.parse(output) as USBStatus & { version: number };
  if (
    !document ||
    document.version !== 1 ||
    !Array.isArray(document.sources) ||
    document.sources.some(
      (source) =>
        !source ||
        typeof source.name !== "string" ||
        !sourcePattern.test(source.name) ||
        (source.error !== undefined && typeof source.error !== "string"),
    ) ||
    !Array.isArray(document.devices)
  ) {
    throw new Error(
      "The Codespace USB command returned an unsupported response",
    );
  }
  for (const device of document.devices) {
    if (
      !device ||
      typeof device.source !== "string" ||
      (device.source !== "" && !sourcePattern.test(device.source)) ||
      typeof device.busId !== "string" ||
      typeof device.description !== "string" ||
      (imported
        ? !Number.isInteger(device.port) ||
          device.port < 0 ||
          device.port > 65535 ||
          typeof device.state !== "string"
        : !busIDPattern.test(device.busId) ||
          !sourcePattern.test(device.source))
    ) {
      throw new Error("The Codespace USB command returned an invalid device");
    }
  }
  return { sources: document.sources, devices: document.devices };
}

export class USBClient extends RuntimeCommand {
  constructor(log: (message: string) => void) {
    super("/usr/local/bin/gitea-codespace-usbip", 35000, log);
  }

  async list(): Promise<USBList> {
    return parseUSBResponse(
      (await this.run(["list", "--format", "json"])).stdout,
      false,
    );
  }

  async status(): Promise<USBStatus> {
    return parseUSBResponse(
      (await this.run(["status", "--format", "json"])).stdout,
      true,
    );
  }

  async attach(busId: string, source: string): Promise<void> {
    if (!busIDPattern.test(busId)) throw new Error("Invalid USB bus ID");
    if (!sourcePattern.test(source)) throw new Error("Invalid USB source");
    await this.run(["attach", busId, "--source", source]);
  }

  async detach(port: number): Promise<void> {
    if (!Number.isInteger(port) || port < 0 || port > 65535)
      throw new Error("Invalid USB port");
    await this.run(["detach", String(port)]);
  }
}
