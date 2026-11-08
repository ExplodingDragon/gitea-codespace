import {ref} from 'vue';
import {currentLocale} from './i18n';

export const csrf = ref('');

export class APIError extends Error {
  constructor(
    readonly code: string,
    readonly status: number,
    message: string,
    readonly detail: string,
  ) {
    super(message);
  }
}

type Translate = (key: string, parameters?: Record<string, unknown>) => string;

export function errorMessage(error: unknown, translate?: Translate): string {
  if (error instanceof APIError && translate) {
    const summary = translate(`errors.${error.code}`, {status: error.status});
    return error.detail ? `${summary}\n${error.detail}` : summary;
  }
  if (error instanceof APIError) return error.detail ? `${error.message}\n${error.detail}` : error.message;
  return error instanceof Error ? error.message : String(error);
}

export async function request<T>(path: string, method = 'GET', body?: unknown): Promise<T> {
  const headers: Record<string, string> = {
    'Accept-Language': currentLocale(),
    'X-CSRF-Token': csrf.value,
  };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const response = await fetch(`/api/admin/${path}`, {
    method,
    credentials: 'same-origin',
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (response.status === 401) {
    csrf.value = '';
  }
  if (!response.ok) {
    const fallback = `Request failed (${response.status})`;
    const data: unknown = await response.json().catch(() => undefined);
    const code =
      typeof data === 'object' && data !== null && 'code' in data && typeof data.code === 'string'
        ? data.code
        : 'request_failed';
    const message =
      typeof data === 'object' && data !== null && 'message' in data && typeof data.message === 'string'
        ? data.message
        : fallback;
    const detail =
      typeof data === 'object' && data !== null && 'detail' in data && typeof data.detail === 'string'
        ? data.detail
        : '';
    throw new APIError(code, response.status, message, detail);
  }
  if (response.status === 204) return undefined as T;
  const text = await response.text();
  return text ? (JSON.parse(text) as T) : (undefined as T);
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
  ready?: boolean;
  pod?: ResourceReference;
  volume?: ResourceReference;
};

export type RuntimeStatus = ResourceStatus & {
  canStop: boolean;
  stopping: boolean;
  stopped: boolean;
  deleting: boolean;
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

export function upsertResource<T, S>(resources: Resource<T, S>[], saved: Resource<T, S>): Resource<T, S>[] {
  const index = resources.findIndex(({uid}) => uid === saved.uid);
  if (index < 0) return [...resources, saved];
  return resources.map((resource, current) => (current === index ? saved : resource));
}

export type Runtime = {
  isolation: string;
  runtimeClassName?: string;
  storageClassName: string;
  storage: {storage: string};
  accessMode: string;
  resources: {requests: Record<string, string>; limits?: Record<string, string>};
  gitSSHKeyType: string;
  devContainer: {
    webIDE: {
      enabled: boolean;
      archiveURL?: string;
      settings: Record<string, unknown>;
      extensions?: string[];
    };
    giteaCLI: boolean;
    features?: {reference: string; options?: Record<string, unknown>}[];
    lifecycleScript?: string;
  };
  proxy?: OutboundProxy;
  native?: NativeRuntime;
};

export type NativeRuntime = {
  runtimeCapAdd: string[];
  devContainerCapAdd: string[];
  devicePlugins: {
    resourceName: string;
    count: number;
    providesGPU?: boolean;
    cdi?: {kind: string; deviceIDEnvironment: string};
    hostAccess?: {
      devices?: {source: string; target: string; permissions: string}[];
      mounts?: {source: string; target: string; readOnly?: boolean}[];
      forwardEnvironment?: string[];
    };
  }[];
};

export type OutboundProxy = {
  httpProxy?: string;
  httpsProxy?: string;
  socksProxy?: string;
  noProxy?: string[];
  caBundle?: string;
};

export type ClusterOptions = {
  runtimeClasses: {name: string; handler: string; isolation: 'kata' | 'sysbox'}[];
  storageClasses: {name: string; provisioner: string; default: boolean}[];
  deviceResources: {name: string; nodes: number; maxPerNode: number}[];
  requiredRuntimeCapabilities: string[];
  ingressClasses: {name: string; controller: string; default: boolean}[];
  tlsSecrets: string[];
  issuers: {kind: 'Issuer' | 'ClusterIssuer'; name: string}[];
};

export type Environment = {
  description: string;
  runtime: Runtime;
};

export type Site = {
  displayName: string;
  url: string;
  caBundle?: string;
  enabled: boolean;
  acceptCreates: boolean;
  startupConcurrency: number;
  cleanupConcurrency: number;
  templates: ResourceReference[];
  gateway: ResourceReference;
  caches: ResourceReference[];
  quota: Record<string, string>;
  upstreams: {cidr: string; ports: number[]}[];
};

export type Component = {
  role: 'gateway' | 'cache';
  displayName: string;
  resources: {requests: Record<string, string>; limits: Record<string, string>};
  gateway?: {
    replicas: number;
    endpointBaseURL: string;
    sshExternalAddress: string;
    httpExposure: {
      mode: 'external' | 'ingress';
      ingress?: {
        className: string;
        tls: {
          source: 'none' | 'secret' | 'certManager';
          secretName?: string;
          issuer?: {kind: 'Issuer' | 'ClusterIssuer'; name: string};
        };
      };
    };
    sshExposure: {mode: 'external' | 'loadBalancer'};
    trustedProxyCIDRs?: string[];
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
    proxy?: OutboundProxy;
  };
};

export type ComponentStatus = {
  available: boolean;
  readyReplicas: number;
  desiredReplicas: number;
  httpExposureConfigured?: boolean;
  sshExposureConfigured?: boolean;
  serviceNamespace?: string;
  serviceName?: string;
  sshLoadBalancerAddresses?: string[];
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
