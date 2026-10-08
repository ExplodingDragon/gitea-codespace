<script setup lang="ts">
import {onMounted, ref, toRaw} from 'vue';
import {NAlert, NButton, NCheckbox, NForm, NFormItem, NInput, NInputNumber, NSelect, NTag, useDialog} from 'naive-ui';
import {KeyRound, Pencil, Plus, RefreshCw, Trash2, X} from '@lucide/vue';
import {request, type Component, type ComponentStatus, type Resource} from '../api';
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
    error.value = (e as Error).message;
  } finally {
    busy.value = false;
  }
}
onMounted(load);

function defaultGateway(): Component {
  return {
    role: 'gateway',
    displayName: '',
    gateway: {
      http: {listen: ':8080', public_url: ''},
      ssh: {
        listen: ':2222',
        public_addr: '',
        handshake_timeout: '30s',
        max_channels_per_connection: 32,
        auth: {
          max_attempts_per_ip_per_minute: 30,
          max_attempts_per_codespace_per_minute: 20,
          max_attempts_per_ip_codespace_per_minute: 10,
          max_attempts_per_public_key_per_minute: 30,
          failure_window: '10m',
        },
      },
      sessions: {ttl: '8h', idle_timeout: '30m', revalidate_interval: '5m', max_per_codespace: 32, max_per_user: 128},
      limits: {
        max_inflight_total: 4096,
        max_inflight_per_session: 32,
        public_max_connections_per_endpoint: 64,
        public_max_connections_per_ip: 16,
        validation_max_inflight: 128,
      },
    },
  };
}
function defaultCache(): Component {
  return {
    role: 'cache',
    displayName: '',
    cache: {
      id: '',
      name: '',
      revision: '0',
      enabled: true,
      listen: ':5000',
      public_url: '',
      storage: {
        driver: 'filesystem',
        path: '/var/lib/codespace-cache',
        min_free_space: '1GiB',
        s3: {endpoint: '', region: '', bucket: '', prefix: '', force_path_style: false},
      },
      max_size: '50GiB',
      max_age: '720h',
      gc_interval: '24h',
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
  upstreams.value = Object.entries(form.value.cache?.upstreams ?? {}).map(([host, config]) => ({
    host,
    allow: config.allow.join('\n'),
  }));
  editing.value = true;
  dirty.saved();
}
function setRole(role: 'gateway' | 'cache') {
  const displayName = form.value?.displayName ?? '';
  form.value = role === 'gateway' ? defaultGateway() : defaultCache();
  form.value.displayName = displayName;
  upstreams.value = [];
}
function close() {
  if (dirty.dirty.value && !window.confirm('Discard unsaved changes?')) return;
  editing.value = false;
  dirty.reset();
}
async function save() {
  busy.value = true;
  error.value = '';
  try {
    const spec = structuredClone(toRaw(form.value!));
    if (spec.cache) {
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
    dirty.reset();
    await load();
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    busy.value = false;
  }
}
function remove(item: Resource<Component, ComponentStatus>) {
  dialog.warning({
    title: `Delete ${item.spec.displayName}?`,
    content: 'Sites must stop referencing this component before it can be deleted.',
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
        error.value = (e as Error).message;
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
        error.value = (e as Error).message;
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
    </div>
    <template v-if="form.gateway">
      <h2>Gateway addresses</h2>
      <div class="fields">
        <NFormItem label="Public HTTPS URL">
          <NInput
            v-model:value="form.gateway.http.public_url"
            placeholder="https://codespace.example.com"
            required
          />
        </NFormItem>
        <NFormItem label="Public SSH address">
          <NInput
            v-model:value="form.gateway.ssh.public_addr"
            placeholder="codespace.example.com:22"
            required
          />
        </NFormItem>
        <NFormItem label="HTTP listen address">
          <NInput
            v-model:value="form.gateway.http.listen"
            required
          />
        </NFormItem>
        <NFormItem label="SSH listen address">
          <NInput
            v-model:value="form.gateway.ssh.listen"
            required
          />
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
            v-model:value="form.gateway.sessions.idle_timeout"
            required
          />
        </NFormItem>
        <NFormItem label="Authorization refresh">
          <NInput
            v-model:value="form.gateway.sessions.revalidate_interval"
            required
          />
        </NFormItem>
        <NFormItem label="Sessions per Codespace">
          <NInputNumber
            v-model:value="form.gateway.sessions.max_per_codespace"
            :min="1"
          />
        </NFormItem>
        <NFormItem label="Sessions per user">
          <NInputNumber
            v-model:value="form.gateway.sessions.max_per_user"
            :min="1"
          />
        </NFormItem>
        <NFormItem label="Total in-flight requests">
          <NInputNumber
            v-model:value="form.gateway.limits.max_inflight_total"
            :min="1"
          />
        </NFormItem>
        <NFormItem label="In-flight requests per session">
          <NInputNumber
            v-model:value="form.gateway.limits.max_inflight_per_session"
            :min="1"
          />
        </NFormItem>
        <NFormItem label="SSH channels per connection">
          <NInputNumber
            v-model:value="form.gateway.ssh.max_channels_per_connection"
            :min="1"
          />
        </NFormItem>
      </div>
    </template>
    <template v-if="form.cache">
      <div class="fields">
        <NFormItem label="Public registry URL">
          <NInput
            v-model:value="form.cache.public_url"
            placeholder="https://cache.example.com"
            required
          />
        </NFormItem>
        <NFormItem label="Listen address">
          <NInput
            v-model:value="form.cache.listen"
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
        <NFormItem label="Maximum size"><NInput v-model:value="form.cache.max_size" /></NFormItem>
        <NFormItem label="Maximum age">
          <NInput
            v-model:value="form.cache.max_age"
            required
          />
        </NFormItem>
        <NFormItem label="Cleanup interval">
          <NInput
            v-model:value="form.cache.gc_interval"
            required
          />
        </NFormItem>
      </div>
      <template v-if="form.cache.storage.driver === 'filesystem'">
        <div class="fields">
          <NFormItem label="Mounted storage path">
            <NInput
              v-model:value="form.cache.storage.path"
              required
            />
          </NFormItem>
          <NFormItem label="Minimum free space"><NInput v-model:value="form.cache.storage.min_free_space" /></NFormItem>
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
            <NCheckbox v-model:checked="form.cache.storage.s3.force_path_style">Use path-style requests</NCheckbox>
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
            {{ item.spec.gateway?.http.public_url || item.spec.cache?.public_url }}
            <p
              v-if="item.spec.gateway"
              class="muted"
            >
              SSH {{ item.spec.gateway.ssh.public_addr }}
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
