import {ref} from 'vue';

export const csrf = ref('');

export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

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
  storageClassName: string;
  storage: {storage: string};
  volumeMode: 'Filesystem' | 'Block';
  accessMode: string;
  resources: {requests: Record<string, string>; limits: Record<string, string>};
  gitSSHKeyType: string;
  devContainer: {
    webIDE: {enabled: boolean; feature?: string; version?: string; extensions?: string[]};
    features?: {reference: string; options?: Record<string, unknown>}[];
  };
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

export type Component = {
  role: 'gateway' | 'cache';
  displayName: string;
  resources: {requests: Record<string, string>; limits: Record<string, string>};
  gateway?: {
    publicURL: string;
    sshPublicAddress: string;
    parent: {name?: string; namespace?: string; httpSectionName?: string; sshSectionName?: string};
    ssh: {
      handshakeTimeout: string;
      maxChannelsPerConnection: number;
      auth: {
        maxAttemptsPerIP: number;
        maxAttemptsPerCodespace: number;
        maxAttemptsPerIPCodespace: number;
        maxAttemptsPerPublicKey: number;
        failureWindow: string;
      };
    };
    sessions: {
      ttl: string;
      idleTimeout: string;
      revalidateInterval: string;
      maxPerCodespace: number;
      maxPerUser: number;
    };
    limits: {
      maxInflightTotal: number;
      maxInflightPerSession: number;
      publicMaxConnectionsPerEndpoint: number;
      publicMaxConnectionsPerIP: number;
      validationMaxInflight: number;
    };
  };
  cache?: {
    enabled: boolean;
    publicURL: string;
    storage: {
      driver: 'filesystem' | 's3';
      volumeSize?: string;
      storageClassName?: string;
      minFreeSpace?: string;
      s3: {endpoint?: string; region?: string; bucket?: string; prefix?: string; forcePathStyle?: boolean};
    };
    maxSize?: string;
    maxAge: string;
    gcInterval: string;
    upstreams: Record<string, {allow: string[]}>;
  };
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
