<script setup lang="ts">
import {onMounted, ref, toRaw} from 'vue';
import {NAlert, NButton, NCheckbox, NForm, NFormItem, NInput, NInputNumber, NSelect, NTag, useDialog} from 'naive-ui';
import {KeyRound, Pencil, Plus, RefreshCw, Trash2, X} from '@lucide/vue';
import {errorMessage, request, type Component, type ComponentStatus, type Resource} from '../api';
import {useDirtyForm} from '../composables/useDirtyForm';

const items = ref<Resource<Component, ComponentStatus>[]>([]);
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
    error.value = errorMessage(e);
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
        if (!host || spec.cache.upstreams[host]) throw new Error('Registry hosts must be present and unique');
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
    error.value = errorMessage(e);
  } finally {
    busy.value = false;
  }
}
function remove(item: Resource<Component, ComponentStatus>) {
  dialog.warning({
    title: `Delete ${item.spec.displayName}?`,
    content:
      item.spec.role === 'cache'
        ? 'Remove site references, disable the cache, and wait for its writer lease to expire before deleting it.'
        : 'Sites must stop referencing this component before it can be deleted.',
    positiveText: 'Delete',
    negativeText: 'Cancel',
    onPositiveClick: async () => {
      try {
        await request(`components/${item.name}`, 'DELETE', {
          name: item.name,
          uid: item.uid,
          resourceVersion: item.resourceVersion,
        });
        await load();
      } catch (e) {
        error.value = errorMessage(e);
      }
    },
  });
}
function rotateHostKey(item: Resource<Component, ComponentStatus>) {
  dialog.warning({
    title: `Rotate the SSH host key for ${item.spec.displayName}?`,
    content: 'SSH access is temporarily unavailable while the Gateway publishes its new host key.',
    positiveText: 'Rotate key',
    negativeText: 'Cancel',
    onPositiveClick: async () => {
      try {
        await request(`components/${item.name}/rotate-ssh-host-key`, 'POST', {
          name: item.name,
          uid: item.uid,
          resourceVersion: item.resourceVersion,
        });
        await load();
      } catch (e) {
        error.value = errorMessage(e);
      }
    },
  });
}
</script>

<template>
  <div class="page-heading">
    <h1>{{ editing ? (selected ? 'Edit component' : 'New component') : 'Components' }}</h1>
    <div
      v-if="!editing"
      class="actions"
    >
      <NButton
        aria-label="Refresh components"
        title="Refresh components"
        :disabled="busy"
        @click="load"
      >
        <template #icon><RefreshCw :size="16" /></template>
      </NButton>
      <NButton
        type="primary"
        @click="edit()"
      >
        Add component
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
      <NFormItem label="Resource name">
        <NInput
          v-model:value="name"
          :disabled="!!selected"
          required
        />
      </NFormItem>
      <NFormItem label="Display name">
        <NInput
          v-model:value="form.displayName"
          required
        />
      </NFormItem>
      <NFormItem label="Role">
        <NSelect
          :value="form.role"
          :disabled="!!selected"
          :options="[
            {label: 'Gateway', value: 'gateway'},
            {label: 'Registry cache', value: 'cache'},
          ]"
          @update:value="setRole"
        />
      </NFormItem>
      <NFormItem label="CPU request">
        <NInput
          v-model:value="form.resources.requests.cpu"
          required
        />
      </NFormItem>
      <NFormItem label="Memory request">
        <NInput
          v-model:value="form.resources.requests.memory"
          required
        />
      </NFormItem>
      <NFormItem label="CPU limit">
        <NInput
          v-model:value="form.resources.limits.cpu"
          required
        />
      </NFormItem>
      <NFormItem label="Memory limit">
        <NInput
          v-model:value="form.resources.limits.memory"
          required
        />
      </NFormItem>
    </div>
    <template v-if="form.gateway">
      <h2>Gateway addresses</h2>
      <div class="fields">
        <NFormItem label="Public HTTPS URL">
          <NInput
            v-model:value="form.gateway.publicURL"
            placeholder="https://codespace.example.com"
            required
          />
        </NFormItem>
        <NFormItem label="Public SSH address">
          <NInput
            v-model:value="form.gateway.sshPublicAddress"
            placeholder="codespace.example.com:22"
            required
          />
        </NFormItem>
        <NFormItem label="Gateway API parent">
          <NInput
            v-model:value="form.gateway.parent.name"
            placeholder="public-gateway"
          />
        </NFormItem>
        <NFormItem
          v-if="form.gateway.parent.name"
          label="Gateway namespace"
        >
          <NInput
            v-model:value="form.gateway.parent.namespace"
            required
          />
        </NFormItem>
        <NFormItem
          v-if="form.gateway.parent.name"
          label="HTTPS listener"
        >
          <NInput
            v-model:value="form.gateway.parent.httpSectionName"
            required
          />
        </NFormItem>
        <NFormItem
          v-if="form.gateway.parent.name"
          label="SSH listener"
        >
          <NInput v-model:value="form.gateway.parent.sshSectionName" />
        </NFormItem>
      </div>
      <h2>Sessions and limits</h2>
      <div class="fields">
        <NFormItem label="Session lifetime">
          <NInput
            v-model:value="form.gateway.sessions.ttl"
            required
          />
        </NFormItem>
        <NFormItem label="Idle timeout">
          <NInput
            v-model:value="form.gateway.sessions.idleTimeout"
            required
          />
        </NFormItem>
        <NFormItem label="Authorization refresh">
          <NInput
            v-model:value="form.gateway.sessions.revalidateInterval"
            required
          />
        </NFormItem>
        <NFormItem label="Sessions per Codespace">
          <NInputNumber
            v-model:value="form.gateway.sessions.maxPerCodespace"
            :min="1"
          />
        </NFormItem>
        <NFormItem label="Sessions per user">
          <NInputNumber
            v-model:value="form.gateway.sessions.maxPerUser"
            :min="1"
          />
        </NFormItem>
        <NFormItem label="Total in-flight requests">
          <NInputNumber
            v-model:value="form.gateway.limits.maxInflightTotal"
            :min="1"
          />
        </NFormItem>
        <NFormItem label="In-flight requests per session">
          <NInputNumber
            v-model:value="form.gateway.limits.maxInflightPerSession"
            :min="1"
          />
        </NFormItem>
        <NFormItem label="SSH channels per connection">
          <NInputNumber
            v-model:value="form.gateway.ssh.maxChannelsPerConnection"
            :min="1"
          />
        </NFormItem>
      </div>
    </template>
    <template v-if="form.cache">
      <div class="fields">
        <NFormItem label="Public registry URL">
          <NInput
            v-model:value="form.cache.publicURL"
            placeholder="https://cache.example.com"
            required
          />
        </NFormItem>
        <NFormItem label="Storage driver">
          <NSelect
            v-model:value="form.cache.storage.driver"
            :options="[
              {label: 'Persistent volume', value: 'filesystem'},
              {label: 'S3-compatible storage', value: 's3'},
            ]"
          />
        </NFormItem>
        <NFormItem label="Maximum cache size"><NInput v-model:value="form.cache.maxSize" /></NFormItem>
        <NFormItem label="Maximum age">
          <NInput
            v-model:value="form.cache.maxAge"
            required
          />
        </NFormItem>
        <NFormItem label="Cleanup interval">
          <NInput
            v-model:value="form.cache.gcInterval"
            required
          />
        </NFormItem>
      </div>
      <template v-if="form.cache.storage.driver === 'filesystem'">
        <div class="fields">
          <NFormItem label="Volume size">
            <NInput
              v-model:value="form.cache.storage.volumeSize"
              required
            />
          </NFormItem>
          <NFormItem label="Storage class"><NInput v-model:value="form.cache.storage.storageClassName" /></NFormItem>
          <NFormItem label="Minimum free space"><NInput v-model:value="form.cache.storage.minFreeSpace" /></NFormItem>
        </div>
      </template>
      <template v-else>
        <div class="fields">
          <NFormItem label="S3 endpoint"><NInput v-model:value="form.cache.storage.s3.endpoint" /></NFormItem>
          <NFormItem label="Region">
            <NInput
              v-model:value="form.cache.storage.s3.region"
              required
            />
          </NFormItem>
          <NFormItem label="Bucket">
            <NInput
              v-model:value="form.cache.storage.s3.bucket"
              required
            />
          </NFormItem>
          <NFormItem label="Prefix"><NInput v-model:value="form.cache.storage.s3.prefix" /></NFormItem>
          <NFormItem label="Replace access key">
            <NInput
              v-model:value="s3AccessKey"
              autocomplete="off"
            />
          </NFormItem>
          <NFormItem label="Replace secret key">
            <NInput
              v-model:value="s3SecretKey"
              type="password"
              autocomplete="new-password"
            />
          </NFormItem>
          <NFormItem>
            <NCheckbox v-model:checked="form.cache.storage.s3.forcePathStyle">Use path-style requests</NCheckbox>
          </NFormItem>
        </div>
      </template>
      <h2>Registry mirrors</h2>
      <p class="muted">
        Each registry host is mirrored only for the listed repository prefixes. A trailing
        <code>*</code>
        allows repositories below that prefix.
      </p>
      <div
        v-for="(upstream, index) in upstreams"
        :key="index"
        class="upstream-row"
      >
        <NInput
          v-model:value="upstream.host"
          placeholder="ghcr.io"
          aria-label="Registry host"
        />
        <NInput
          v-model:value="upstream.allow"
          type="textarea"
          :autosize="{minRows: 1, maxRows: 4}"
          placeholder="devcontainers/*"
          aria-label="Allowed repositories"
        />
        <NButton
          aria-label="Remove registry mirror"
          title="Remove registry mirror"
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
        Add registry mirror
      </NButton>
      <NFormItem><NCheckbox v-model:checked="form.cache.enabled">Accept cache requests</NCheckbox></NFormItem>
    </template>
    <div class="form-actions">
      <NButton
        type="primary"
        attr-type="submit"
        :loading="busy"
      >
        Save component
      </NButton>
      <NButton
        :disabled="busy"
        @click="close"
      >
        Cancel
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
          <th>Component</th>
          <th>Role</th>
          <th>Public address</th>
          <th>State</th>
          <th>Actions</th>
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
          <td>{{ item.spec.role === 'gateway' ? 'Gateway' : 'Registry cache' }}</td>
          <td>
            {{ item.spec.gateway?.publicURL || item.spec.cache?.publicURL }}
            <p
              v-if="item.spec.gateway"
              class="muted"
            >
              SSH {{ item.spec.gateway.sshPublicAddress }}
            </p>
          </td>
          <td>
            <NTag
              v-if="item.deleting"
              size="small"
              type="warning"
            >
              Deleting
            </NTag>
            <NTag
              v-else-if="item.spec.cache && !item.spec.cache.enabled"
              size="small"
            >
              Disabled
            </NTag>
            <NTag
              v-else
              size="small"
              :type="item.status?.available ? 'success' : 'warning'"
            >
              {{ item.status?.available ? 'Available' : 'Pending' }}
            </NTag>
            <p
              v-if="item.spec.cache && item.status?.lastHeartbeat"
              class="muted"
            >
              Cache heartbeat received
            </p>
          </td>
          <td>
            <div class="actions">
              <NButton
                v-if="item.spec.gateway"
                aria-label="Rotate SSH host key"
                title="Rotate SSH host key"
                :disabled="item.deleting"
                @click="rotateHostKey(item)"
              >
                <template #icon><KeyRound :size="16" /></template>
              </NButton>
              <NButton
                aria-label="Edit component"
                title="Edit component"
                :disabled="item.deleting"
                @click="edit(item)"
              >
                <template #icon><Pencil :size="16" /></template>
              </NButton>
              <NButton
                aria-label="Delete component"
                title="Delete component"
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
            {{ busy ? 'Loading components...' : 'No components configured' }}
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
