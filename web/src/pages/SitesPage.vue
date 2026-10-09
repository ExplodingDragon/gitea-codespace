<script setup lang="ts">
import {onMounted, ref, toRaw} from 'vue';
import {NAlert, NButton, NCheckbox, NForm, NFormItem, NInput, NInputNumber, NSelect, useDialog} from 'naive-ui';
import {Pencil, Trash2, RefreshCw} from '@lucide/vue';
import {request, type Component, type Environment, type Resource, type Site} from '../api';
import {useDirtyForm} from '../composables/useDirtyForm';
import ResourceStatus from '../components/ResourceStatus.vue';

const sites = ref<Resource<Site>[]>([]);
const environments = ref<Resource<Environment>[]>([]);
const components = ref<Resource<Component>[]>([]);
const editing = ref(false);
const selected = ref<Resource<Site>>();
const form = ref<Site>();
const name = ref('');
const secret = ref('');
const error = ref('');
const busy = ref(false);
const dialog = useDialog();
const dirty = useDirtyForm(() => ({name: name.value, form: form.value, secret: secret.value}));
const quotaFields = [
  {key: 'pods', label: 'Pods'},
  {key: 'persistentvolumeclaims', label: 'Persistent volumes'},
  {key: 'requests.cpu', label: 'CPU requests'},
  {key: 'limits.cpu', label: 'CPU limits'},
  {key: 'requests.memory', label: 'Memory requests'},
  {key: 'limits.memory', label: 'Memory limits'},
  {key: 'requests.storage', label: 'Storage'},
];
async function load() {
  busy.value = true;
  error.value = '';
  try {
    sites.value = await request('sites');
    environments.value = await request('environments');
    components.value = await request('components');
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    busy.value = false;
  }
}
onMounted(load);
function edit(site?: Resource<Site>) {
  selected.value = site;
  name.value = site?.name ?? '';
  secret.value = '';
  error.value = '';
  form.value = site
    ? structuredClone(toRaw(site.spec))
    : {
        displayName: '',
        url: '',
        managerID: '',
        credential: {name: '', uid: ''},
        enabled: false,
        acceptCreates: true,
        startupConcurrency: 1,
        cleanupConcurrency: 16,
        templates: [],
        gateway: {name: '', uid: ''},
        caches: [],
        quota: {
          pods: '10',
          persistentvolumeclaims: '10',
          'requests.cpu': '10',
          'limits.cpu': '20',
          'requests.memory': '10Gi',
          'limits.memory': '20Gi',
          'requests.storage': '100Gi',
        },
        containerLimits: {type: 'Container'},
        upstreams: [],
      };
  form.value.caches ||= [];
  form.value.upstreams ||= [];
  editing.value = true;
  dirty.beginTracking();
}
function close() {
  if (!dirty.confirmDiscard()) return;
  editing.value = false;
  secret.value = '';
  dirty.endTracking();
}
async function save() {
  busy.value = true;
  error.value = '';
  try {
    await request(`sites${selected.value ? `/${name.value}` : ''}`, selected.value ? 'PUT' : 'POST', {
      name: name.value,
      uid: selected.value?.uid ?? '',
      resourceVersion: selected.value?.resourceVersion ?? '',
      spec: form.value,
      managerSecret: secret.value,
    });
    secret.value = '';
    editing.value = false;
    dirty.endTracking();
    await load();
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    busy.value = false;
  }
}
function remove(site: Resource<Site>) {
  dialog.warning({
    title: `Delete ${site.spec.displayName}?`,
    content: 'The site namespace is removed after its resources have been cleaned up.',
    positiveText: 'Delete',
    negativeText: 'Cancel',
    onPositiveClick: async () => {
      try {
        await request(`sites/${site.name}`, 'DELETE', {
          name: site.name,
          uid: site.uid,
          resourceVersion: site.resourceVersion,
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
    <h1>{{ editing ? (selected ? 'Edit Gitea site' : 'New Gitea site') : 'Gitea sites' }}</h1>
    <div
      v-if="!editing"
      class="actions"
    >
      <NButton
        aria-label="Refresh sites"
        title="Refresh sites"
        :disabled="busy"
        @click="load"
      >
        <template #icon><RefreshCw :size="16" /></template>
      </NButton>
      <NButton
        type="primary"
        @click="edit()"
      >
        Add site
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
      <NFormItem label="Gitea URL">
        <NInput
          v-model:value="form.url"
          placeholder="https://gitea.example.com"
          required
        />
      </NFormItem>
      <NFormItem label="Manager ID">
        <NInput
          v-model:value="form.managerID"
          :disabled="!!selected"
          placeholder="Manager ID from Gitea"
          required
        />
      </NFormItem>
    </div>
    <NFormItem :label="selected ? 'Replace Manager secret (configured)' : 'Manager secret'">
      <NInput
        v-model:value="secret"
        type="password"
        autocomplete="new-password"
        placeholder="Leave blank to retain the configured secret"
        :required="!selected"
      />
    </NFormItem>
    <div class="fields">
      <NFormItem label="Environment templates">
        <NSelect
          multiple
          :value="form.templates.map((item) => item.uid)"
          :options="
            environments.map((item) => ({
              label: `${item.spec.tag} (${item.name})`,
              value: item.uid,
              disabled: item.deleting,
            }))
          "
          @update:value="
            (values: string[]) => {
              form!.templates = environments
                .filter((item) => values.includes(item.uid))
                .map(({name, uid}) => ({name, uid}));
            }
          "
        />
      </NFormItem>
      <NFormItem label="Gateway">
        <NSelect
          :value="form.gateway.uid || null"
          :options="
            components
              .filter((item) => item.spec.role === 'gateway')
              .map((item) => ({label: item.name, value: item.uid}))
          "
          @update:value="
            (value: string) => {
              const item = components.find((item) => item.uid === value)!;
              form!.gateway = {name: item.name, uid: item.uid};
            }
          "
        />
      </NFormItem>
      <NFormItem label="Caches">
        <NSelect
          multiple
          :value="form.caches.map((item) => item.uid)"
          :options="
            components.filter((item) => item.spec.role === 'cache').map((item) => ({label: item.name, value: item.uid}))
          "
          @update:value="
            (values: string[]) => {
              form!.caches = components.filter((item) => values.includes(item.uid)).map(({name, uid}) => ({name, uid}));
            }
          "
        />
      </NFormItem>
      <NFormItem label="Lifecycle">
        <div class="checkboxes">
          <NCheckbox v-model:checked="form.enabled">Enabled</NCheckbox>
          <NCheckbox v-model:checked="form.acceptCreates">Accept new Codespaces</NCheckbox>
        </div>
      </NFormItem>
      <NFormItem label="Concurrent startups">
        <NInputNumber
          v-model:value="form.startupConcurrency"
          :min="1"
          :max="64"
        />
      </NFormItem>
      <NFormItem label="Concurrent cleanup operations">
        <NInputNumber
          v-model:value="form.cleanupConcurrency"
          :min="1"
          :max="64"
        />
      </NFormItem>
    </div>
    <h2>Site quota</h2>
    <div class="fields">
      <NFormItem
        v-for="field in quotaFields"
        :key="field.key"
        :label="field.label"
      >
        <NInput
          v-model:value="form.quota[field.key]"
          required
        />
      </NFormItem>
    </div>
    <h2>Upstream networks</h2>
    <div
      v-for="(upstream, index) in form.upstreams"
      :key="index"
      class="upstream-row"
    >
      <NFormItem label="CIDR">
        <NInput
          v-model:value="upstream.cidr"
          placeholder="10.0.0.10/32"
        />
      </NFormItem>
      <NFormItem label="TCP ports">
        <NSelect
          :value="upstream.ports"
          multiple
          filterable
          tag
          :options="upstream.ports.map((port) => ({label: String(port), value: port}))"
          @update:value="
            (values: (string | number)[]) => {
              upstream.ports = values.map(Number);
            }
          "
        />
      </NFormItem>
      <NButton
        quaternary
        aria-label="Remove upstream"
        title="Remove upstream"
        @click="form.upstreams.splice(index, 1)"
      >
        <template #icon><Trash2 :size="16" /></template>
      </NButton>
    </div>
    <NButton @click="form.upstreams.push({cidr: '', ports: [443]})">Add upstream</NButton>
    <div class="form-actions">
      <NButton
        type="primary"
        attr-type="submit"
        :loading="busy"
      >
        Save site
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
          <th>Site</th>
          <th>Gitea</th>
          <th>Availability</th>
          <th>Actions</th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="site in sites"
          :key="site.uid"
        >
          <td>
            <strong>{{ site.spec.displayName }}</strong>
            <p class="muted">{{ site.name }}</p>
          </td>
          <td>
            {{ site.spec.url }}
            <p class="muted">Manager {{ site.spec.managerID }}</p>
          </td>
          <td><ResourceStatus :resource="site" /></td>
          <td>
            <div class="actions">
              <NButton
                aria-label="Edit site"
                title="Edit site"
                :disabled="site.deleting"
                @click="edit(site)"
              >
                <template #icon><Pencil :size="16" /></template>
              </NButton>
              <NButton
                aria-label="Delete site"
                title="Delete site"
                :disabled="site.spec.enabled || site.deleting"
                @click="remove(site)"
              >
                <template #icon><Trash2 :size="16" /></template>
              </NButton>
            </div>
          </td>
        </tr>
        <tr v-if="!sites.length">
          <td
            colspan="4"
            class="empty"
          >
            {{ busy ? 'Loading sites...' : 'No Gitea sites configured' }}
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
