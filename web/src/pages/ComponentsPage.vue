<script setup lang="ts">
import {onMounted, ref, toRaw} from 'vue';
import {NAlert, NButton, NCheckbox, NForm, NFormItem, NInput, NInputNumber, NSelect, NTag, useDialog} from 'naive-ui';
import {KeyRound, Pencil, Plus, RefreshCw, Trash2, X} from '@lucide/vue';
import {useI18n} from 'vue-i18n';
import {errorMessage, request, type Component, type ComponentStatus, type Resource} from '../api';
import {useDirtyForm} from '../composables/useDirtyForm';

const items = ref<Resource<Component, ComponentStatus>[]>([]);
const {t} = useI18n();
const selected = ref<Resource<Component, ComponentStatus>>();
const editing = ref(false);
const name = ref('');
const form = ref<Component>();
const s3AccessKey = ref('');
const s3SecretKey = ref('');
const upstreams = ref<{host: string; allow: string}[]>([]);
const error = ref('');
const busy = ref(false);
const dialog = useDialog();
const dirty = useDirtyForm(() => ({
  name: name.value,
  form: form.value,
  s3AccessKey: s3AccessKey.value,
  s3SecretKey: s3SecretKey.value,
  upstreams: upstreams.value,
}));

async function load() {
  busy.value = true;
  error.value = '';
  try {
    items.value = await request('components');
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    busy.value = false;
  }
}
onMounted(load);

function defaultGateway(): Component {
  return {
    role: 'gateway',
    displayName: '',
    resources: {requests: {cpu: '50m', memory: '64Mi'}, limits: {cpu: '1', memory: '512Mi'}},
    gateway: {
      publicURL: '',
      sshPublicAddress: '',
      parent: {name: '', namespace: '', httpSectionName: '', sshSectionName: ''},
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
      publicURL: '',
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
  selected.value = item;
  name.value = item?.name ?? '';
  error.value = '';
  s3AccessKey.value = '';
  s3SecretKey.value = '';
  form.value = item ? structuredClone(toRaw(item.spec)) : defaultGateway();
  if (form.value.gateway) {
    form.value.gateway.parent = {
      name: '',
      namespace: '',
      httpSectionName: '',
      sshSectionName: '',
      ...form.value.gateway.parent,
    };
  }
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
}
function close() {
  if (!dirty.confirmDiscard()) return;
  editing.value = false;
  dirty.endTracking();
}
async function save() {
  busy.value = true;
  error.value = '';
  try {
    const spec = structuredClone(toRaw(form.value!));
    if (spec.gateway && !spec.gateway.parent.name?.trim()) {
      spec.gateway.parent = {name: '', namespace: '', httpSectionName: '', sshSectionName: ''};
    }
    if (spec.cache) {
      if (spec.cache.storage.driver === 'filesystem') {
        spec.cache.storage.s3 = {};
      } else {
        spec.cache.storage.volumeSize = undefined;
        spec.cache.storage.storageClassName = undefined;
        spec.cache.storage.minFreeSpace = undefined;
      }
      spec.cache.upstreams = {};
      for (const item of upstreams.value) {
        const host = item.host.trim().toLowerCase();
        if (!host || spec.cache.upstreams[host]) throw new Error(t('component.duplicateRegistry'));
        spec.cache.upstreams[host] = {
          allow: item.allow
            .split(/[\n,]/)
            .map((value) => value.trim())
            .filter(Boolean),
        };
      }
    }
    await request(`components${selected.value ? `/${name.value}` : ''}`, selected.value ? 'PUT' : 'POST', {
      name: name.value,
      uid: selected.value?.uid ?? '',
      resourceVersion: selected.value?.resourceVersion ?? '',
      spec,
      s3AccessKey: s3AccessKey.value,
      s3SecretKey: s3SecretKey.value,
    });
    editing.value = false;
    dirty.endTracking();
    await load();
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    busy.value = false;
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
        :disabled="busy"
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
    <div class="fields">
      <NFormItem :label="t('common.resourceName')">
        <NInput
          v-model:value="name"
          :disabled="!!selected"
          required
        />
      </NFormItem>
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
      <NFormItem :label="t('component.cpuRequest')">
        <NInput
          v-model:value="form.resources.requests.cpu"
          required
        />
      </NFormItem>
      <NFormItem :label="t('component.memoryRequest')">
        <NInput
          v-model:value="form.resources.requests.memory"
          required
        />
      </NFormItem>
      <NFormItem :label="t('component.cpuLimit')">
        <NInput
          v-model:value="form.resources.limits.cpu"
          required
        />
      </NFormItem>
      <NFormItem :label="t('component.memoryLimit')">
        <NInput
          v-model:value="form.resources.limits.memory"
          required
        />
      </NFormItem>
    </div>
    <template v-if="form.gateway">
      <h2>{{ t('component.gatewayAddresses') }}</h2>
      <div class="fields">
        <NFormItem :label="t('component.publicHTTPS')">
          <NInput
            v-model:value="form.gateway.publicURL"
            placeholder="https://codespace.example.com"
            required
          />
        </NFormItem>
        <NFormItem :label="t('component.publicSSH')">
          <NInput
            v-model:value="form.gateway.sshPublicAddress"
            placeholder="codespace.example.com:22"
            required
          />
        </NFormItem>
        <NFormItem :label="t('component.gatewayParent')">
          <NInput
            v-model:value="form.gateway.parent.name"
            placeholder="public-gateway"
          />
        </NFormItem>
        <NFormItem
          v-if="form.gateway.parent.name"
          :label="t('component.gatewayNamespace')"
        >
          <NInput
            v-model:value="form.gateway.parent.namespace"
            required
          />
        </NFormItem>
        <NFormItem
          v-if="form.gateway.parent.name"
          :label="t('component.httpListener')"
        >
          <NInput
            v-model:value="form.gateway.parent.httpSectionName"
            required
          />
        </NFormItem>
        <NFormItem
          v-if="form.gateway.parent.name"
          :label="t('component.sshListener')"
        >
          <NInput v-model:value="form.gateway.parent.sshSectionName" />
        </NFormItem>
      </div>
      <h2>{{ t('component.sessions') }}</h2>
      <div class="fields">
        <NFormItem :label="t('component.sessionLifetime')">
          <NInput
            v-model:value="form.gateway.sessions.ttl"
            required
          />
        </NFormItem>
        <NFormItem :label="t('component.idleTimeout')">
          <NInput
            v-model:value="form.gateway.sessions.idleTimeout"
            required
          />
        </NFormItem>
        <NFormItem :label="t('component.authRefresh')">
          <NInput
            v-model:value="form.gateway.sessions.revalidateInterval"
            required
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
      <div class="fields">
        <NFormItem :label="t('component.publicRegistry')">
          <NInput
            v-model:value="form.cache.publicURL"
            placeholder="https://cache.example.com"
            required
          />
        </NFormItem>
        <NFormItem :label="t('component.storageDriver')">
          <NSelect
            v-model:value="form.cache.storage.driver"
            :options="[
              {label: t('component.persistentVolume'), value: 'filesystem'},
              {label: t('component.s3Storage'), value: 's3'},
            ]"
          />
        </NFormItem>
        <NFormItem :label="t('component.maxCacheSize')"><NInput v-model:value="form.cache.maxSize" /></NFormItem>
        <NFormItem :label="t('component.maxAge')">
          <NInput
            v-model:value="form.cache.maxAge"
            required
          />
        </NFormItem>
        <NFormItem :label="t('component.cleanupInterval')">
          <NInput
            v-model:value="form.cache.gcInterval"
            required
          />
        </NFormItem>
      </div>
      <template v-if="form.cache.storage.driver === 'filesystem'">
        <div class="fields">
          <NFormItem :label="t('component.volumeSize')">
            <NInput
              v-model:value="form.cache.storage.volumeSize"
              required
            />
          </NFormItem>
          <NFormItem :label="t('component.storageClass')">
            <NInput v-model:value="form.cache.storage.storageClassName" />
          </NFormItem>
          <NFormItem :label="t('component.minFreeSpace')">
            <NInput v-model:value="form.cache.storage.minFreeSpace" />
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
        :loading="busy"
      >
        {{ t('component.save') }}
      </NButton>
      <NButton
        :disabled="busy"
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
          <th>{{ t('component.publicAddress') }}</th>
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
            {{ item.spec.gateway?.publicURL || item.spec.cache?.publicURL }}
            <p
              v-if="item.spec.gateway"
              class="muted"
            >
              {{ t('component.sshAddress', {address: item.spec.gateway.sshPublicAddress}) }}
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
              :type="item.status?.available ? 'success' : 'warning'"
            >
              {{ item.status?.available ? t('common.available') : t('common.pending') }}
            </NTag>
            <p
              v-if="item.spec.cache && item.status?.lastHeartbeat"
              class="muted"
            >
              {{ t('component.heartbeat') }}
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
            {{ busy ? t('component.loading') : t('component.empty') }}
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
