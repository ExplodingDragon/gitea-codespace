export interface CodespaceEndpoint {
  port: number;
  label: string;
  visibility: "authenticated" | "public";
}

export interface CodespaceUSBDevice {
  source: string;
  busId: string;
  description: string;
  port?: number;
  state?: string;
}

export interface CodespaceUSBState {
  version: number;
  sources: { name: string; error?: string }[];
  devices: CodespaceUSBDevice[];
}

export interface CodespaceCapabilities {
  ports: boolean;
  usb: boolean;
  ssh?: { user: string; host: string; port: number };
}
