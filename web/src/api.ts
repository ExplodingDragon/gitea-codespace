import {ref} from 'vue';

export const csrf = ref('');

export async function request<T>(path: string, method = 'GET', body?: unknown): Promise<T> {
  const response = await fetch(`/api/admin/${path}`, {
    method,
    credentials: 'same-origin',
    headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrf.value},
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (response.status === 401) {
    csrf.value = '';
  }
  if (!response.ok) {
    const fallback = `Request failed (${response.status})`;
    const data: unknown = await response.json().catch(() => undefined);
    const message =
      typeof data === 'object' && data !== null && 'error' in data && typeof data.error === 'string'
        ? data.error
        : fallback;
    throw new Error(message);
  }
  return response.status === 204 ? (undefined as T) : response.json();
}

export type ResourceReference = {
  name: string;
  uid: string;
};

export type Condition = {
  type: string;
  status: string;
  reason: string;
  message: string;
  observedGeneration?: number;
};

type ResourceStatus = {
  conditions?: Condition[];
  verifiedGeneration?: number;
  verification?: string;
  ready?: boolean;
  pod?: ResourceReference;
  volume?: ResourceReference;
};

export type Resource<T, S = ResourceStatus> = {
  name: string;
  namespace?: string;
  uid: string;
  resourceVersion: string;
  generation: number;
  deleting: boolean;
  spec: T;
  status?: S;
};

export type Runtime = {
  isolation: string;
  runtimeClassName: string;
  image: string;
  storageClassName: string;
  storage: {storage: string};
  volumeMode: 'Filesystem' | 'Block';
  accessMode: string;
  resources: {requests: Record<string, string>; limits: Record<string, string>};
  gitSSHKeyType: string;
  codeServerVersion: string;
};

export type Environment = {
  tag: string;
  description: string;
  runtime: Runtime;
};

export type Site = {
  displayName: string;
  url: string;
  managerID: string;
  credential: ResourceReference;
  enabled: boolean;
  acceptCreates: boolean;
  startupConcurrency: number;
  cleanupConcurrency: number;
  templates: ResourceReference[];
  gateway: ResourceReference;
  caches: ResourceReference[];
  quota: Record<string, string>;
  containerLimits: {
    type: string;
    default?: Record<string, string>;
    defaultRequest?: Record<string, string>;
    min?: Record<string, string>;
    max?: Record<string, string>;
    maxLimitRequestRatio?: Record<string, string>;
  };
  upstreams: {cidr: string; ports: number[]}[];
};

export type Gateway = {
  http: {listen: string; public_url: string};
  ssh: {
    listen: string;
    public_addr: string;
    handshake_timeout: string;
    max_channels_per_connection: number;
    auth: {
      max_attempts_per_ip_per_minute: number;
      max_attempts_per_codespace_per_minute: number;
      max_attempts_per_ip_codespace_per_minute: number;
      max_attempts_per_public_key_per_minute: number;
      failure_window: string;
    };
  };
  sessions: {
    ttl: string;
    idle_timeout: string;
    revalidate_interval: string;
    max_per_codespace: number;
    max_per_user: number;
  };
  limits: {
    max_inflight_total: number;
    max_inflight_per_session: number;
    public_max_connections_per_endpoint: number;
    public_max_connections_per_ip: number;
    validation_max_inflight: number;
  };
};

export type Cache = {
  id: string;
  name: string;
  revision: string;
  enabled: boolean;
  listen: string;
  public_url: string;
  storage: {
    driver: 'filesystem' | 's3';
    path: string;
    min_free_space: string;
    s3: {endpoint: string; region: string; bucket: string; prefix: string; force_path_style: boolean};
  };
  max_size: string;
  max_age: string;
  gc_interval: string;
  upstreams: Record<string, {allow: string[]}>;
};

export type Component = {
  role: 'gateway' | 'cache';
  displayName: string;
  gateway?: Gateway;
  cache?: Cache;
};

export type ComponentStatus = {
  available: boolean;
  readyReplicas: number;
  desiredReplicas: number;
  lastHeartbeat?: string;
  cacheBytes?: number;
  mirrorBytes?: number;
  cleanupResult?: string;
};

export type RuntimeInstance = {
  site: ResourceReference;
  codespaceID: string;
  runtimeUUID: string;
  environmentTag: string;
  operation: string;
  version: number;
};
