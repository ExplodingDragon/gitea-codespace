<script setup lang="ts">
import {computed, onMounted, ref, toRaw} from 'vue';
import {NAlert, NButton, NCheckbox, NForm, NFormItem, NInput, NInputNumber, NSelect, NTag, useDialog} from 'naive-ui';
import {KeyRound, Pencil, Plus, RefreshCw, Trash2, X} from '@lucide/vue';
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
import MeasurementInput from '../components/MeasurementInput.vue';
import {measurementBaseValue} from '../utils/measurement';

const items = ref<Resource<Component, ComponentStatus>[]>([]);
const cluster = ref<ClusterOptions>({
  runtimeClasses: [],
  storageClasses: [],
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
const ingressClassOptions = computed(() =>
  cluster.value.ingressClasses.map(({name: className, controller, default: isDefault}) => ({
    label: `${className} (${isDefault ? t('component.defaultIngressClass') : controller})`,
    value: className,
  })),
);
const tlsSecretOptions = computed(() =>
  cluster.value.tlsSecrets.map((secretName) => ({label: secretName, value: secretName})),
);
const issuerOptions = computed(() =>
  cluster.value.issuers.map(({kind, name: issuerName}) => ({
    label: `${kind}/${issuerName}`,
    value: `${kind}/${issuerName}`,
  })),
);
const storageOptions = computed(() => {
  const hasDefaultStorage = cluster.value.storageClasses.some(({default: isDefault}) => isDefault);
  const options = [
    {
      label: hasDefaultStorage
        ? t('component.clusterDefaultStorage')
        : t('common.unavailableOption', {name: t('component.clusterDefaultStorage')}),
      value: '__default__',
      disabled: !hasDefaultStorage,
    },
    ...cluster.value.storageClasses.map(({name: storageName, default: isDefault}) => ({
      label: isDefault ? `${storageName} (${t('environment.defaultStorage')})` : storageName,
      value: storageName,
      disabled: false,
    })),
  ];
  const current = form.value?.cache?.storage.storageClassName;
  if (current && !options.some(({value}) => value === current))
    options.push({label: t('common.unavailableOption', {name: current}), value: current, disabled: true});
  return options;
});

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
  form.value = item ? structuredClone(toRaw(item.spec)) : defaultGateway();
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
function setHTTPExposure(value: 'external' | 'ingress') {
  if (!form.value?.gateway) return;
  form.value.gateway.httpExposure =
    value === 'external'
      ? {mode: 'external'}
      : {
          mode: 'ingress',
          ingress: {
            className: cluster.value.ingressClasses.find(({default: isDefault}) => isDefault)?.name ?? '',
            tls: {source: form.value.gateway.endpointBaseURL.startsWith('https://') ? 'secret' : 'none'},
          },
        };
}
function setTLS(source: 'none' | 'secret' | 'certManager') {
  if (!form.value?.gateway?.httpExposure.ingress) return;
  form.value.gateway.httpExposure.ingress.tls = {source};
}
function setIssuer(value: string) {
  const tls = form.value?.gateway?.httpExposure.ingress?.tls;
  if (!tls) return;
  const [kind, ...nameParts] = value.split('/');
  tls.issuer = {kind: kind as 'Issuer' | 'ClusterIssuer', name: nameParts.join('/')};
}
function setSSHExposure(value: 'external' | 'loadBalancer') {
  if (!form.value?.gateway) return;
  form.value.gateway.sshExposure = {mode: value};
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
    const spec = structuredClone(toRaw(form.value!));
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
      class="actions"
    >
      <NButton
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
    <div class="fields">
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
    <div class="fields">
      <NFormItem
        v-if="form.gateway"
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
      <h2>{{ t('component.gatewayAddresses') }}</h2>
      <div class="fields">
        <NFormItem :label="t('component.publicHTTPS')">
          <NInput
            v-model:value="form.gateway.endpointBaseURL"
            placeholder="https://codespace.example.com"
            required
          />
        </NFormItem>
        <NFormItem :label="t('component.publicSSH')">
          <NInput
            v-model:value="form.gateway.sshExternalAddress"
            placeholder="codespace.example.com:22"
            required
          />
        </NFormItem>
      </div>
      <h2>{{ t('component.publicExposure') }}</h2>
      <p class="section-help">{{ t('component.publicExposureHelp') }}</p>
      <div class="fields">
        <NFormItem :label="t('component.httpExposure')">
          <NSelect
            :value="form.gateway.httpExposure.mode"
            :options="[
              {label: t('component.externalExposure'), value: 'external'},
              {label: t('component.ingressExposure'), value: 'ingress'},
            ]"
            @update:value="setHTTPExposure"
          />
        </NFormItem>
        <NFormItem :label="t('component.sshExposure')">
          <NSelect
            :value="form.gateway.sshExposure.mode"
            :options="[
              {label: t('component.externalExposure'), value: 'external'},
              {label: t('component.loadBalancerExposure'), value: 'loadBalancer'},
            ]"
            @update:value="setSSHExposure"
          />
        </NFormItem>
      </div>
      <div
        v-if="form.gateway.httpExposure.ingress"
        class="fields"
      >
        <NFormItem :label="t('component.ingressClass')">
          <NSelect
            v-model:value="form.gateway.httpExposure.ingress.className"
            :options="ingressClassOptions"
          />
        </NFormItem>
        <NFormItem :label="t('component.tlsSource')">
          <NSelect
            :value="form.gateway.httpExposure.ingress.tls.source"
            :options="[
              {label: t('component.noTLS'), value: 'none'},
              {label: t('component.existingTLSSecret'), value: 'secret'},
              {label: t('component.certManagerIssuer'), value: 'certManager'},
            ]"
            @update:value="setTLS"
          />
        </NFormItem>
        <NFormItem
          v-if="form.gateway.httpExposure.ingress.tls.source === 'secret'"
          :label="t('component.tlsSecret')"
        >
          <NSelect
            v-model:value="form.gateway.httpExposure.ingress.tls.secretName"
            :options="tlsSecretOptions"
          />
        </NFormItem>
        <NFormItem
          v-if="form.gateway.httpExposure.ingress.tls.source === 'certManager'"
          :label="t('component.issuer')"
        >
          <NSelect
            :value="
              form.gateway.httpExposure.ingress.tls.issuer
                ? `${form.gateway.httpExposure.ingress.tls.issuer.kind}/${form.gateway.httpExposure.ingress.tls.issuer.name}`
                : ''
            "
            :options="issuerOptions"
            @update:value="setIssuer"
          />
        </NFormItem>
      </div>
      <NFormItem
        :label="t('component.trustedProxies')"
        :feedback="t('component.trustedProxiesHelp')"
      >
        <NInput
          v-model:value="trustedProxyCIDRs"
          type="textarea"
          :autosize="{minRows: 2, maxRows: 5}"
          placeholder="10.0.0.0/8"
        />
      </NFormItem>
      <h2>{{ t('component.sessions') }}</h2>
      <div class="fields">
        <NFormItem :label="t('component.sessionLifetime')">
          <MeasurementInput
            v-model="form.gateway.sessions.ttl"
            kind="duration"
            :aria-label="t('component.sessionLifetime')"
          />
        </NFormItem>
        <NFormItem :label="t('component.idleTimeout')">
          <MeasurementInput
            v-model="form.gateway.sessions.idleTimeout"
            kind="duration"
            :aria-label="t('component.idleTimeout')"
          />
        </NFormItem>
        <NFormItem :label="t('component.authRefresh')">
          <MeasurementInput
            v-model="form.gateway.sessions.revalidateInterval"
            kind="duration"
            :aria-label="t('component.authRefresh')"
          />
        </NFormItem>
        <NFormItem :label="t('component.sessionsPerCodespace')">
          <NInputNumber
            v-model:value="form.gateway.sessions.maxPerCodespace"
            :min="1"
          />
        </NFormItem>
        <NFormItem :label="t('component.sessionsPerUser')">
          <NInputNumber
            v-model:value="form.gateway.sessions.maxPerUser"
            :min="1"
          />
        </NFormItem>
        <NFormItem :label="t('component.inflightTotal')">
          <NInputNumber
            v-model:value="form.gateway.limits.maxInflightTotal"
            :min="1"
          />
        </NFormItem>
        <NFormItem :label="t('component.inflightSession')">
          <NInputNumber
            v-model:value="form.gateway.limits.maxInflightPerSession"
            :min="1"
          />
        </NFormItem>
        <NFormItem :label="t('component.sshChannels')">
          <NInputNumber
            v-model:value="form.gateway.ssh.maxChannelsPerConnection"
            :min="1"
          />
        </NFormItem>
      </div>
    </template>
    <template v-if="form.cache">
      <h2>{{ t('component.cacheSection') }}</h2>
      <div class="fields">
        <NFormItem :label="t('component.storageDriver')">
          <NSelect
            v-model:value="form.cache.storage.driver"
            :options="[
              {label: t('component.persistentVolume'), value: 'filesystem'},
              {label: t('component.s3Storage'), value: 's3'},
            ]"
          />
        </NFormItem>
        <NFormItem
          :label="t('component.maxCacheSize')"
          :feedback="t('component.maxCacheSizeHelp')"
        >
          <MeasurementInput
            v-model="form.cache.maxSize"
            kind="bytes"
            :aria-label="t('component.maxCacheSize')"
            :allow-empty="form.cache.storage.driver === 's3'"
          />
        </NFormItem>
        <NFormItem :label="t('component.maxAge')">
          <MeasurementInput
            v-model="form.cache.maxAge"
            kind="duration"
            :aria-label="t('component.maxAge')"
          />
        </NFormItem>
        <NFormItem :label="t('component.cleanupInterval')">
          <MeasurementInput
            v-model="form.cache.gcInterval"
            kind="duration"
            :aria-label="t('component.cleanupInterval')"
          />
        </NFormItem>
      </div>
      <template v-if="form.cache.storage.driver === 'filesystem'">
        <div class="fields">
          <NFormItem
            :label="t('component.volumeSize')"
            :feedback="t('component.volumeSizeHelp')"
          >
            <MeasurementInput
              v-model="form.cache.storage.volumeSize"
              kind="storage"
              :aria-label="t('component.volumeSize')"
            />
          </NFormItem>
          <NFormItem
            :label="t('component.storageClass')"
            :feedback="t('component.storageClassHelp')"
          >
            <NSelect
              :value="form.cache.storage.storageClassName || '__default__'"
              :options="storageOptions"
              @update:value="
                (value: string) => {
                  form!.cache!.storage.storageClassName = value === '__default__' ? '' : value;
                }
              "
            />
          </NFormItem>
          <NFormItem :label="t('component.minFreeSpace')">
            <MeasurementInput
              v-model="form.cache.storage.minFreeSpace"
              kind="bytes"
              :aria-label="t('component.minFreeSpace')"
            />
          </NFormItem>
        </div>
      </template>
      <template v-else>
        <div class="fields">
          <NFormItem :label="t('component.s3Endpoint')">
            <NInput v-model:value="form.cache.storage.s3.endpoint" />
          </NFormItem>
          <NFormItem :label="t('component.region')">
            <NInput
              v-model:value="form.cache.storage.s3.region"
              required
            />
          </NFormItem>
          <NFormItem :label="t('component.bucket')">
            <NInput
              v-model:value="form.cache.storage.s3.bucket"
              required
            />
          </NFormItem>
          <NFormItem :label="t('component.prefix')"><NInput v-model:value="form.cache.storage.s3.prefix" /></NFormItem>
          <NFormItem :label="t('component.replaceAccessKey')">
            <NInput
              v-model:value="s3AccessKey"
              autocomplete="off"
            />
          </NFormItem>
          <NFormItem :label="t('component.replaceSecretKey')">
            <NInput
              v-model:value="s3SecretKey"
              type="password"
              autocomplete="new-password"
            />
          </NFormItem>
          <NFormItem>
            <NCheckbox v-model:checked="form.cache.storage.s3.forcePathStyle">{{ t('component.pathStyle') }}</NCheckbox>
          </NFormItem>
        </div>
      </template>
      <h2>{{ t('component.mirrors') }}</h2>
      <p class="muted">{{ t('component.mirrorHelp') }}</p>
      <div
        v-for="(upstream, index) in upstreams"
        :key="index"
        class="upstream-row"
      >
        <NInput
          v-model:value="upstream.host"
          placeholder="ghcr.io"
          :aria-label="t('component.registryHost')"
        />
        <NInput
          v-model:value="upstream.allow"
          type="textarea"
          :autosize="{minRows: 1, maxRows: 4}"
          placeholder="devcontainers/*"
          :aria-label="t('component.allowedRepositories')"
        />
        <NButton
          :aria-label="t('component.removeMirror')"
          :title="t('component.removeMirror')"
          @click="upstreams.splice(index, 1)"
        >
          <template #icon><X :size="16" /></template>
        </NButton>
      </div>
      <NButton
        class="add-row"
        @click="upstreams.push({host: '', allow: ''})"
      >
        <template #icon><Plus :size="16" /></template>
        {{ t('component.addMirror') }}
      </NButton>
      <NFormItem>
        <NCheckbox v-model:checked="form.cache.enabled">{{ t('component.acceptCache') }}</NCheckbox>
      </NFormItem>
    </template>
    <div class="form-actions">
      <NButton
        type="primary"
        attr-type="submit"
        :loading="saving"
      >
        {{ t('component.save') }}
      </NButton>
      <NButton
        :disabled="saving"
        @click="close"
      >
        {{ t('common.cancel') }}
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
          <th>{{ t('common.actions') }}</th>
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
          <td>
            <div class="actions">
              <NButton
                v-if="item.spec.gateway"
                :aria-label="t('component.rotateKey')"
                :title="t('component.rotateKey')"
                :disabled="item.deleting"
                @click="rotateHostKey(item)"
              >
                <template #icon><KeyRound :size="16" /></template>
              </NButton>
              <NButton
                :aria-label="t('component.editAction')"
                :title="t('component.editAction')"
                :disabled="item.deleting"
                @click="edit(item)"
              >
                <template #icon><Pencil :size="16" /></template>
              </NButton>
              <NButton
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
