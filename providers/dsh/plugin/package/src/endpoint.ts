import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import type { Endpoint, EndpointVisibility } from './types.ts'

const execFileAsync = promisify(execFile)
const ENDPOINT_CLI = '/usr/local/bin/gitea-codespace-endpoint'
const MAX_OUTPUT_BYTES = 1024 * 1024
const TIMEOUT_MS = 5000

/** Subprocess boundary used by the endpoint client and its focused tests. */
export type EndpointCommandRunner = (
  args: readonly string[],
  signal?: AbortSignal,
) => Promise<{ stdout: string; stderr: string }>

interface EndpointDocument {
  version?: unknown
  endpoints?: unknown
}

/** Validate and detach the versioned endpoint CLI response. */
export function parseEndpointList(output: string): Endpoint[] {
  let document: EndpointDocument
  try {
    document = JSON.parse(output) as EndpointDocument
  } catch {
    throw new Error('The Codespace endpoint command returned invalid JSON')
  }
  if (document.version !== 1 || !Array.isArray(document.endpoints)) {
    throw new Error('The Codespace endpoint command returned an unsupported response')
  }
  const ports = new Set<number>()
  const endpoints = document.endpoints.map((value): Endpoint => {
    if (typeof value !== 'object' || value === null || Array.isArray(value)) {
      throw new Error('The Codespace endpoint command returned an invalid endpoint')
    }
    const endpoint = value as Partial<Endpoint>
    validatePort(endpoint.port)
    if (typeof endpoint.label !== 'string' || !isVisibility(endpoint.visibility)) {
      throw new Error('The Codespace endpoint command returned an invalid endpoint')
    }
    const normalized = validateEndpoint({ port: endpoint.port, label: endpoint.label, visibility: endpoint.visibility })
    if (ports.has(normalized.port)) {
      throw new Error(`The Codespace endpoint command returned duplicate port ${normalized.port}`)
    }
    ports.add(normalized.port)
    return normalized
  })
  return endpoints.toSorted((left, right) => left.port - right.port)
}

/** Runtime endpoint operations shared by Remote, commands, and tools. */
export class EndpointClient {
  constructor(private readonly run: EndpointCommandRunner = runEndpointCommand) {}

  /** Read the authoritative endpoint set. */
  async list(signal?: AbortSignal): Promise<Endpoint[]> {
    const { stdout } = await this.run(['list', '--format', 'json'], signal)
    return parseEndpointList(stdout)
  }

  /** Add or replace one endpoint. */
  async publish(endpoint: Endpoint, signal?: AbortSignal): Promise<Endpoint> {
    const normalized = validateEndpoint(endpoint)
    const args = ['set', String(normalized.port), '--label', normalized.label]
    if (normalized.visibility === 'public') args.push('--public')
    await this.run(args, signal)
    return normalized
  }

  /** Remove one endpoint by port. */
  async unpublish(port: number, signal?: AbortSignal): Promise<number> {
    validatePort(port)
    await this.run(['delete', String(port)], signal)
    return port
  }
}

function validateEndpoint(endpoint: Endpoint): Endpoint {
  validatePort(endpoint.port)
  const label = endpoint.label.trim()
  const labelBytes = new TextEncoder().encode(label).length
  if (labelBytes < 1 || labelBytes > 128) throw new Error('Endpoint label must contain between 1 and 128 UTF-8 bytes')
  if (!isVisibility(endpoint.visibility)) throw new Error('Endpoint visibility must be authenticated or public')
  return { port: endpoint.port, label, visibility: endpoint.visibility }
}

function validatePort(port: unknown): asserts port is number {
  if (typeof port !== 'number' || !Number.isSafeInteger(port) || port < 1 || port > 65535) {
    throw new Error('Endpoint port must be an integer between 1 and 65535')
  }
}

function isVisibility(value: unknown): value is EndpointVisibility {
  return value === 'authenticated' || value === 'public'
}

async function runEndpointCommand(args: readonly string[], signal?: AbortSignal): Promise<{ stdout: string; stderr: string }> {
  const result = await execFileAsync(ENDPOINT_CLI, args, {
    encoding: 'utf8',
    maxBuffer: MAX_OUTPUT_BYTES,
    signal,
    timeout: TIMEOUT_MS,
  })
  return { stdout: result.stdout, stderr: result.stderr }
}
