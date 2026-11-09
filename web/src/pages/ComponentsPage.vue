<script setup lang="ts">
import {onMounted, ref} from 'vue';
import {NAlert, NButton, NForm, NFormItem, NInput, NInputNumber, NSelect, NTag, useDialog} from 'naive-ui';
import {KeyRound, Pencil, RefreshCw, Trash2} from '@lucide/vue';
import {useI18n} from 'vue-i18n';
import {
  errorMessage,
  request,
  upsertResource,
  type ClusterOptions,
  type Component,
  type ComponentStatus,
  type Resource,
} from '../api';
import {useDirtyForm} from '../composables/useDirtyForm';
import CacheFields from '../components/CacheFields.vue';
import GatewayFields from '../components/GatewayFields.vue';
import MeasurementInput from '../components/MeasurementInput.vue';
import {cloneFormValue} from '../utils/form';
import {ingressEndpointURL, loadBalancerSSHAddress} from '../utils/gateway';
import {measurementBaseValue} from '../utils/measurement';

const items = ref<Resource<Component, ComponentStatus>[]>([]);
const cluster = ref<ClusterOptions>({
  runtimeClasses: [],
  storageClasses: [],
  deviceResources: [],
  requiredRuntimeCapabilities: [],
  ingressClasses: [],
  tlsSecrets: [],
  issuers: [],
});
const {t} = useI18n();
const selected = ref<Resource<Component, ComponentStatus>>();
const editing = ref(false);
const name = ref('');
const form = ref<Component>();
const s3AccessKey = ref('');
const s3SecretKey = ref('');
const upstreams = ref<{host: string; allow: string}[]>([]);
const trustedProxyCIDRs = ref('');
const error = ref('');
const loading = ref(false);
const saving = ref(false);
const dialog = useDialog();
const dirty = useDirtyForm(() => ({
  form: form.value,
  s3AccessKey: s3AccessKey.value,
  s3SecretKey: s3SecretKey.value,
  upstreams: upstreams.value,
  trustedProxyCIDRs: trustedProxyCIDRs.value,
}));
async function load() {
  loading.value = true;
  error.value = '';
  try {
    [items.value, cluster.value] = await Promise.all([
      request<Resource<Component, ComponentStatus>[]>('components'),
      request<ClusterOptions>('cluster-options'),
    ]);
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    loading.value = false;
  }
}
onMounted(load);

function defaultGateway(): Component {
  return {
    role: 'gateway',
    displayName: '',
    resources: {requests: {cpu: '50m', memory: '64Mi'}, limits: {cpu: '1', memory: '512Mi'}},
    gateway: {
      replicas: 2,
      endpointBaseURL: '',
      sshExternalAddress: '',
      httpExposure: {mode: 'external'},
      sshExposure: {mode: 'external'},
      trustedProxyCIDRs: [],
      ssh: {
        handshakeTimeout: '30s',
        maxChannelsPerConnection: 32,
        auth: {
          maxAttemptsPerIP: 30,
          maxAttemptsPerCodespace: 20,
          maxAttemptsPerIPCodespace: 10,
          maxAttemptsPerPublicKey: 30,
          failureWindow: '10m',
        },
      },
      sessions: {ttl: '8h', idleTimeout: '30m', revalidateInterval: '5m', maxPerCodespace: 32, maxPerUser: 128},
      limits: {
        maxInflightTotal: 4096,
        maxInflightPerSession: 32,
        publicMaxConnectionsPerEndpoint: 64,
        publicMaxConnectionsPerIP: 16,
        validationMaxInflight: 128,
      },
    },
  };
}
function defaultCache(): Component {
  return {
    role: 'cache',
    displayName: '',
    resources: {requests: {cpu: '50m', memory: '64Mi'}, limits: {cpu: '1', memory: '512Mi'}},
    cache: {
      enabled: true,
      storage: {
        driver: 'filesystem',
        volumeSize: '50Gi',
        storageClassName: '',
        minFreeSpace: '1GiB',
        s3: {endpoint: '', region: '', bucket: '', prefix: '', forcePathStyle: false},
      },
      maxSize: '45GiB',
      maxAge: '720h',
      gcInterval: '24h',
      upstreams: {},
    },
  };
}
function edit(item?: Resource<Component, ComponentStatus>) {
  const requests = item?.spec.resources.requests;
  const limits = item?.spec.resources.limits;
  if (item && (!requests?.cpu || !requests.memory || !limits?.cpu || !limits.memory)) {
    error.value = t('component.invalidResources');
    return;
  }
  selected.value = item;
  name.value = item?.name ?? '';
  error.value = '';
  s3AccessKey.value = '';
  s3SecretKey.value = '';
  form.value = item ? cloneFormValue(item.spec) : defaultGateway();
  trustedProxyCIDRs.value = form.value.gateway?.trustedProxyCIDRs?.join('\n') ?? '';
  upstreams.value = Object.entries(form.value.cache?.upstreams ?? {}).map(([host, config]) => ({
    host,
    allow: config.allow.join('\n'),
  }));
  editing.value = true;
  dirty.beginTracking();
}
function setRole(role: 'gateway' | 'cache') {
  const displayName = form.value?.displayName ?? '';
  const resources = form.value?.resources;
  form.value = role === 'gateway' ? defaultGateway() : defaultCache();
  form.value.displayName = displayName;
  if (resources) form.value.resources = resources;
  upstreams.value = [];
  trustedProxyCIDRs.value = '';
}
function close() {
  if (!dirty.confirmDiscard()) return;
  editing.value = false;
  dirty.endTracking();
}
async function save() {
  saving.value = true;
  error.value = '';
  try {
    const spec = cloneFormValue(form.value!);
    for (const kind of ['cpu', 'memory'] as const) {
      const requested = measurementBaseValue(spec.resources.requests[kind], kind);
      const limit = measurementBaseValue(spec.resources.limits[kind], kind);
      if (requested === null || limit === null || requested <= 0 || requested > limit)
        throw new Error(t('component.invalidResourceRange', {resource: t(`units.${kind}`)}));
    }
    if (spec.gateway) {
      if (!Number.isInteger(spec.gateway.replicas) || spec.gateway.replicas < 1 || spec.gateway.replicas > 64)
        throw new Error(t('component.invalidReplicas'));
      spec.gateway.trustedProxyCIDRs = trustedProxyCIDRs.value
        .split(/[\n,]/)
        .map((value) => value.trim())
        .filter(Boolean);
      const ingress = spec.gateway.httpExposure.ingress;
      if (spec.gateway.httpExposure.mode === 'ingress') {
        if (!ingress || !cluster.value.ingressClasses.some(({name: className}) => className === ingress.className))
          throw new Error(t('component.invalidIngressClass'));
        spec.gateway.endpointBaseURL = ingressEndpointURL(spec.gateway.endpointBaseURL, ingress.tls.source);
        let ingressPort = '';
        try {
          ingressPort = new URL(spec.gateway.endpointBaseURL).port;
        } catch {
          ingressPort = '';
        }
        if (ingressPort) throw new Error(t('component.invalidIngressPort'));
        if (ingress.tls.source === 'secret' && !cluster.value.tlsSecrets.includes(ingress.tls.secretName ?? ''))
          throw new Error(t('component.invalidTLSSecret'));
        if (
          ingress.tls.source === 'certManager' &&
          !cluster.value.issuers.some(
            ({kind, name: issuerName}) => kind === ingress.tls.issuer?.kind && issuerName === ingress.tls.issuer?.name,
          )
        )
          throw new Error(t('component.invalidIssuer'));
      }
      if (spec.gateway.sshExposure.mode === 'loadBalancer')
        spec.gateway.sshExternalAddress = loadBalancerSSHAddress(spec.gateway.sshExternalAddress);
    }
    if (spec.cache) {
      if (spec.cache.storage.driver === 'filesystem') {
        spec.cache.storage.s3 = {};
        const storageClassName = spec.cache.storage.storageClassName;
        const storageClassAvailable = storageClassName
          ? cluster.value.storageClasses.some(({name: storageName}) => storageName === storageClassName)
          : cluster.value.storageClasses.some(({default: isDefault}) => isDefault);
        if (!storageClassAvailable) throw new Error(t('component.invalidStorageClass'));
        const volume = measurementBaseValue(spec.cache.storage.volumeSize, 'storage');
        const maximum = measurementBaseValue(spec.cache.maxSize, 'bytes');
        const reserve = measurementBaseValue(spec.cache.storage.minFreeSpace, 'bytes');
        if (volume === null || maximum === null || reserve === null || maximum + reserve > volume * 1024)
          throw new Error(t('component.invalidCacheSizes'));
      } else {
        spec.cache.storage.volumeSize = undefined;
        spec.cache.storage.storageClassName = undefined;
        spec.cache.storage.minFreeSpace = undefined;
      }
      if (
        (measurementBaseValue(spec.cache.maxAge, 'duration') ?? 0) <= 0 ||
        (measurementBaseValue(spec.cache.gcInterval, 'duration') ?? 0) <= 0
      )
        throw new Error(t('component.invalidDuration'));
      spec.cache.upstreams = {};
      for (const item of upstreams.value) {
        const host = item.host.trim().toLowerCase();
        if (!host || spec.cache.upstreams[host]) throw new Error(t('component.duplicateRegistry'));
        const allow = item.allow
          .split(/[\n,]/)
          .map((value) => value.trim())
          .filter(Boolean);
        if (!allow.length) throw new Error(t('component.emptyMirrorRepositories'));
        spec.cache.upstreams[host] = {
          allow,
        };
      }
    }
    const saved = await request<Resource<Component, ComponentStatus>>(
      `components${selected.value ? `/${name.value}` : ''}`,
      selected.value ? 'PUT' : 'POST',
      {
        name: selected.value ? name.value : '',
        uid: selected.value?.uid ?? '',
        resourceVersion: selected.value?.resourceVersion ?? '',
        spec,
        s3AccessKey: s3AccessKey.value,
        s3SecretKey: s3SecretKey.value,
      },
    );
    items.value = upsertResource(items.value, saved);
    editing.value = false;
    dirty.endTracking();
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    saving.value = false;
  }
}
function remove(item: Resource<Component, ComponentStatus>) {
  dialog.warning({
    title: t('component.deleteTitle', {name: item.spec.displayName}),
    content: item.spec.role === 'cache' ? t('component.deleteCacheContent') : t('component.deleteGatewayContent'),
    positiveText: t('common.delete'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      try {
        await request(`components/${item.name}`, 'DELETE', {
          name: item.name,
          uid: item.uid,
          resourceVersion: item.resourceVersion,
        });
        await load();
      } catch (e) {
        error.value = errorMessage(e, t);
      }
    },
  });
}
function rotateHostKey(item: Resource<Component, ComponentStatus>) {
  dialog.warning({
    title: t('component.rotateTitle', {name: item.spec.displayName}),
    content: t('component.rotateContent'),
    positiveText: t('component.rotateConfirm'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      try {
        await request(`components/${item.name}/rotate-ssh-host-key`, 'POST', {
          name: item.name,
          uid: item.uid,
          resourceVersion: item.resourceVersion,
        });
        await load();
      } catch (e) {
        error.value = errorMessage(e, t);
      }
    },
  });
}
</script>

<template>
  <div class="page-heading">
    <h1>{{ editing ? (selected ? t('component.edit') : t('component.new')) : t('component.title') }}</h1>
    <div
      v-if="!editing"
      class="page-actions"
    >
      <NButton
        quaternary
        :aria-label="t('component.refresh')"
        :title="t('component.refresh')"
        :disabled="loading"
        @click="load"
      >
        <template #icon><RefreshCw :size="16" /></template>
      </NButton>
      <NButton
        type="primary"
        @click="edit()"
      >
        {{ t('component.add') }}
      </NButton>
    </div>
  </div>
  <NAlert
    v-if="error"
    type="error"
    class="notice"
  >
    {{ error }}
  </NAlert>
  <NForm
    v-if="editing && form"
    class="editor"
    @submit.prevent="save"
  >
    <h2>{{ t('component.basicSection') }}</h2>
    <div class="form-grid">
      <NFormItem :label="t('component.displayName')">
        <NInput
          v-model:value="form.displayName"
          required
        />
      </NFormItem>
      <NFormItem :label="t('component.role')">
        <NSelect
          :value="form.role"
          :disabled="!!selected"
          :options="[
            {label: t('component.gateway'), value: 'gateway'},
            {label: t('component.cache'), value: 'cache'},
          ]"
          @update:value="setRole"
        />
      </NFormItem>
    </div>
    <h2>{{ t('component.computeSection') }}</h2>
    <p class="section-help">{{ t('component.computeHelp') }}</p>
    <div class="form-grid">
      <NFormItem
        v-if="form.gateway"
        class="field-wide"
        :label="t('component.gatewayReplicas')"
        :feedback="t('component.gatewayReplicasHelp')"
      >
        <NInputNumber
          v-model:value="form.gateway.replicas"
          :min="1"
          :max="64"
          :precision="0"
        />
      </NFormItem>
      <NFormItem :label="t('component.cpuRequest')">
        <MeasurementInput
          v-model="form.resources.requests.cpu"
          kind="cpu"
          :aria-label="t('component.cpuRequest')"
        />
      </NFormItem>
      <NFormItem :label="t('component.memoryRequest')">
        <MeasurementInput
          v-model="form.resources.requests.memory"
          kind="memory"
          :aria-label="t('component.memoryRequest')"
        />
      </NFormItem>
      <NFormItem :label="t('component.cpuLimit')">
        <MeasurementInput
          v-model="form.resources.limits.cpu"
          kind="cpu"
          :aria-label="t('component.cpuLimit')"
        />
      </NFormItem>
      <NFormItem :label="t('component.memoryLimit')">
        <MeasurementInput
          v-model="form.resources.limits.memory"
          kind="memory"
          :aria-label="t('component.memoryLimit')"
        />
      </NFormItem>
    </div>
    <template v-if="form.gateway">
      <GatewayFields
        :gateway="form.gateway"
        :cluster="cluster"
        v-model:trusted-proxies="trustedProxyCIDRs"
      />
    </template>
    <template v-if="form.cache">
      <CacheFields
        :cache="form.cache"
        :cluster="cluster"
        v-model:s3-access-key="s3AccessKey"
        v-model:s3-secret-key="s3SecretKey"
        v-model:upstreams="upstreams"
      />
    </template>
    <div class="form-actions">
      <NButton
        :disabled="saving"
        @click="close"
      >
        {{ t('common.cancel') }}
      </NButton>
      <NButton
        type="primary"
        attr-type="submit"
        :loading="saving"
      >
        {{ t('component.save') }}
      </NButton>
    </div>
  </NForm>
  <div
    v-else
    class="table-scroll"
  >
    <table>
      <thead>
        <tr>
          <th>{{ t('component.component') }}</th>
          <th>{{ t('component.role') }}</th>
          <th>{{ t('component.address') }}</th>
          <th>{{ t('component.state') }}</th>
          <th class="table-actions">{{ t('common.actions') }}</th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="item in items"
          :key="item.uid"
        >
          <td>
            <strong>{{ item.spec.displayName }}</strong>
            <p class="muted">{{ item.name }}</p>
          </td>
          <td>{{ item.spec.role === 'gateway' ? t('component.gateway') : t('component.cache') }}</td>
          <td>
            <template v-if="item.spec.gateway">{{ item.spec.gateway.endpointBaseURL }}</template>
            <template v-else-if="item.status?.serviceName">
              https://{{ item.status.serviceName }}.{{ item.status.serviceNamespace }}.svc
            </template>
            <p
              v-if="item.spec.gateway"
              class="muted"
            >
              {{ t('component.sshAddress', {address: item.spec.gateway.sshExternalAddress}) }}
            </p>
            <p
              v-if="item.spec.gateway && item.status?.serviceName"
              class="muted"
            >
              {{
                t('component.serviceAddress', {namespace: item.status.serviceNamespace, name: item.status.serviceName})
              }}
            </p>
            <p
              v-if="item.spec.gateway && item.status?.sshLoadBalancerAddresses?.length"
              class="muted"
            >
              {{ t('component.sshLoadBalancerAddress', {address: item.status.sshLoadBalancerAddresses.join(', ')}) }}
            </p>
          </td>
          <td>
            <NTag
              v-if="item.deleting"
              size="small"
              type="warning"
            >
              {{ t('common.deleting') }}
            </NTag>
            <NTag
              v-else-if="item.spec.cache && !item.spec.cache.enabled"
              size="small"
            >
              {{ t('common.disabled') }}
            </NTag>
            <NTag
              v-else
              size="small"
              :type="
                item.status?.available && item.status.readyReplicas >= item.status.desiredReplicas
                  ? 'success'
                  : 'warning'
              "
            >
              {{
                item.status?.available
                  ? item.status.readyReplicas < item.status.desiredReplicas
                    ? t('component.degraded')
                    : t('common.available')
                  : t('common.pending')
              }}
            </NTag>
            <p
              v-if="item.spec.gateway && item.status"
              class="muted"
            >
              {{
                t('component.replicaState', {
                  ready: item.status.readyReplicas,
                  desired: item.status.desiredReplicas,
                })
              }}
            </p>
            <p
              v-if="item.spec.cache && item.status?.lastHeartbeat"
              class="muted"
            >
              {{ t('component.heartbeat') }}
            </p>
            <p
              v-if="item.spec.gateway"
              class="muted"
            >
              {{
                t('component.publicationState', {
                  http: item.status?.httpExposureConfigured ? t('common.available') : t('common.pending'),
                  ssh: item.status?.sshExposureConfigured ? t('common.available') : t('common.pending'),
                })
              }}
            </p>
          </td>
          <td class="table-actions">
            <div class="table-action-buttons">
              <NButton
                v-if="item.spec.gateway"
                size="small"
                quaternary
                :aria-label="t('component.rotateKey')"
                :title="t('component.rotateKey')"
                :disabled="item.deleting"
                @click="rotateHostKey(item)"
              >
                <template #icon><KeyRound :size="16" /></template>
              </NButton>
              <NButton
                size="small"
                quaternary
                :aria-label="t('component.editAction')"
                :title="t('component.editAction')"
                :disabled="item.deleting"
                @click="edit(item)"
              >
                <template #icon><Pencil :size="16" /></template>
              </NButton>
              <NButton
                size="small"
                quaternary
                type="error"
                :aria-label="t('component.deleteAction')"
                :title="t('component.deleteAction')"
                :disabled="item.deleting"
                @click="remove(item)"
              >
                <template #icon><Trash2 :size="16" /></template>
              </NButton>
            </div>
          </td>
        </tr>
        <tr v-if="!items.length">
          <td
            colspan="5"
            class="empty"
          >
            {{ loading ? t('component.loading') : t('component.empty') }}
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
