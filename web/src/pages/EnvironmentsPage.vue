<script setup lang="ts">
import {onMounted, ref, toRaw} from 'vue';
import {NAlert, NButton, NForm, NFormItem, NInput, NSelect, useDialog} from 'naive-ui';
import {Pencil, Trash2, RefreshCw} from '@lucide/vue';
import {request, type Environment, type Resource} from '../api';
import {useDirtyForm} from '../composables/useDirtyForm';
import ResourceStatus from '../components/ResourceStatus.vue';

const items = ref<Resource<Environment>[]>([]);
const editing = ref(false);
const selected = ref<Resource<Environment>>();
const form = ref<Environment>();
const name = ref('');
const verification = ref('');
const error = ref('');
const busy = ref(false);
const dialog = useDialog();
const dirty = useDirtyForm(() => ({name: name.value, form: form.value, verification: verification.value}));
async function load() {
  busy.value = true;
  error.value = '';
  try {
    items.value = await request('environments');
  } catch (e) {
    error.value = (e as Error).message;
  } finally {
    busy.value = false;
  }
}
onMounted(load);
function edit(item?: Resource<Environment>) {
  selected.value = item;
  name.value = item?.name ?? '';
  verification.value = '';
  error.value = '';
  form.value = item
    ? structuredClone(toRaw(item.spec))
    : {
        tag: '',
        description: '',
        runtime: {
          isolation: 'kata',
          runtimeClassName: 'kata',
          image: '',
          storageClassName: '',
          storage: {storage: '20Gi'},
          volumeMode: 'Block',
          accessMode: 'ReadWriteOncePod',
          resources: {requests: {cpu: '1', memory: '1Gi'}, limits: {cpu: '2', memory: '2Gi'}},
          gitSSHKeyType: 'ed25519',
          codeServerVersion: '',
        },
      };
  editing.value = true;
  dirty.saved();
}
function close() {
  if (dirty.dirty.value && !window.confirm('Discard unsaved changes?')) return;
  editing.value = false;
  dirty.reset();
}
function setIsolation(value: string) {
  if (!form.value) return;
  form.value.runtime.isolation = value;
  form.value.runtime.volumeMode = value === 'kata' ? 'Block' : 'Filesystem';
}
async function save() {
  busy.value = true;
  error.value = '';
  try {
    await request(`environments${selected.value ? `/${name.value}` : ''}`, selected.value ? 'PUT' : 'POST', {
      name: name.value,
      uid: selected.value?.uid ?? '',
      resourceVersion: selected.value?.resourceVersion ?? '',
      spec: form.value,
      verification: verification.value,
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
function remove(item: Resource<Environment>) {
  dialog.warning({
    title: `Delete ${item.name}?`,
    content: 'Templates selected by a site must be removed from that site first.',
    positiveText: 'Delete',
    negativeText: 'Cancel',
    onPositiveClick: async () => {
      try {
        await request(`environments/${item.name}`, 'DELETE', {
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
    <h1>
      {{ editing ? (selected ? 'Edit environment template' : 'New environment template') : 'Environment templates' }}
    </h1>
    <div
      v-if="!editing"
      class="actions"
    >
      <NButton
        aria-label="Refresh templates"
        title="Refresh templates"
        :disabled="busy"
        @click="load"
      >
        <template #icon><RefreshCw :size="16" /></template>
      </NButton>
      <NButton
        type="primary"
        @click="edit()"
      >
        Add template
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
      <NFormItem label="Tag">
        <NInput
          v-model:value="form.tag"
          required
        />
      </NFormItem>
    </div>
    <NFormItem label="Description"><NInput v-model:value="form.description" /></NFormItem>
    <div class="fields">
      <NFormItem label="Isolation">
        <NSelect
          :value="form.runtime.isolation"
          :options="[
            {label: 'Kata', value: 'kata'},
            {label: 'Sysbox', value: 'sysbox'},
          ]"
          @update:value="setIsolation"
        />
      </NFormItem>
      <NFormItem label="RuntimeClass">
        <NInput
          v-model:value="form.runtime.runtimeClassName"
          required
        />
      </NFormItem>
    </div>
    <NFormItem label="Runtime image digest">
      <NInput
        v-model:value="form.runtime.image"
        placeholder="registry.example.com/codespace/runtime@sha256:..."
        required
      />
    </NFormItem>
    <div class="fields">
      <NFormItem label="StorageClass">
        <NInput
          v-model:value="form.runtime.storageClassName"
          required
        />
      </NFormItem>
      <NFormItem label="Volume mode">
        <NSelect
          v-model:value="form.runtime.volumeMode"
          :options="[
            {label: 'Raw block (Kata)', value: 'Block'},
            {label: 'Filesystem (Sysbox)', value: 'Filesystem'},
          ]"
        />
      </NFormItem>
      <NFormItem label="Volume access">
        <NSelect
          v-model:value="form.runtime.accessMode"
          :options="[
            {label: 'ReadWriteOncePod', value: 'ReadWriteOncePod'},
            {label: 'ReadWriteOnce', value: 'ReadWriteOnce'},
          ]"
        />
      </NFormItem>
      <NFormItem label="Storage">
        <NInput
          v-model:value="form.runtime.storage.storage"
          required
        />
      </NFormItem>
      <NFormItem label="Git SSH key type">
        <NSelect
          v-model:value="form.runtime.gitSSHKeyType"
          :options="[
            {label: 'Ed25519', value: 'ed25519'},
            {label: 'RSA 4096', value: 'rsa-4096'},
          ]"
        />
      </NFormItem>
      <NFormItem label="CPU request">
        <NInput
          v-model:value="form.runtime.resources.requests.cpu"
          required
        />
      </NFormItem>
      <NFormItem label="CPU limit">
        <NInput
          v-model:value="form.runtime.resources.limits.cpu"
          required
        />
      </NFormItem>
      <NFormItem label="Memory request">
        <NInput
          v-model:value="form.runtime.resources.requests.memory"
          required
        />
      </NFormItem>
      <NFormItem label="Memory limit">
        <NInput
          v-model:value="form.runtime.resources.limits.memory"
          required
        />
      </NFormItem>
      <NFormItem label="code-server version">
        <NInput
          v-model:value="form.runtime.codeServerVersion"
          required
        />
      </NFormItem>
    </div>
    <NFormItem label="Administrator validation reference">
      <NInput
        v-model:value="verification"
        placeholder="Test run URL or reference for this exact configuration"
      />
    </NFormItem>
    <div class="form-actions">
      <NButton
        type="primary"
        attr-type="submit"
        :loading="busy"
      >
        Save template
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
          <th>Template</th>
          <th>Runtime</th>
          <th>Resources</th>
          <th>Verification</th>
          <th>Actions</th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="item in items"
          :key="item.uid"
        >
          <td>
            <strong>{{ item.spec.tag }}</strong>
            <p class="muted">{{ item.spec.description }}</p>
            <p class="muted">{{ item.name }}</p>
          </td>
          <td>
            {{ item.spec.runtime.isolation }}
            <p class="muted">{{ item.spec.runtime.runtimeClassName }}</p>
          </td>
          <td>
            {{ item.spec.runtime.resources.limits.cpu }} CPU / {{ item.spec.runtime.resources.limits.memory }}
            <p class="muted">{{ item.spec.runtime.storage.storage }} / {{ item.spec.runtime.storageClassName }}</p>
          </td>
          <td><ResourceStatus :resource="item" /></td>
          <td>
            <div class="actions">
              <NButton
                aria-label="Edit template"
                title="Edit template"
                :disabled="item.deleting"
                @click="edit(item)"
              >
                <template #icon><Pencil :size="16" /></template>
              </NButton>
              <NButton
                aria-label="Delete template"
                title="Delete template"
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
            {{ busy ? 'Loading templates...' : 'No environment templates configured' }}
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
