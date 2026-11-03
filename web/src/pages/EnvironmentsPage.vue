<script setup lang="ts">
import {computed, onMounted, ref, toRaw} from 'vue';
import {NAlert, NButton, NCheckbox, NForm, NFormItem, NInput, NSelect, useDialog} from 'naive-ui';
import {Pencil, RefreshCw, Trash2} from '@lucide/vue';
import {useI18n} from 'vue-i18n';
import {errorMessage, request, type ClusterOptions, type Environment, type Resource} from '../api';
import {useDirtyForm} from '../composables/useDirtyForm';
import JsonEditor from '../components/JsonEditor.vue';
import ResourceStatus from '../components/ResourceStatus.vue';

const {t} = useI18n();
const items = ref<Resource<Environment>[]>([]);
const cluster = ref<ClusterOptions>({runtimeClasses: [], storageClasses: []});
const editing = ref(false);
const selected = ref<Resource<Environment>>();
const form = ref<Environment>();
const name = ref('');
const verification = ref('');
const extensionsJSON = ref('[]');
const featuresJSON = ref('{}');
const error = ref('');
const busy = ref(false);
const dialog = useDialog();
const dirty = useDirtyForm(() => ({
  name: name.value,
  form: form.value,
  verification: verification.value,
  extensions: extensionsJSON.value,
  features: featuresJSON.value,
}));

const isolationOptions = computed(() => {
  const available = new Set(cluster.value.runtimeClasses.map(({isolation}) => isolation));
  return [
    {label: 'Kata', value: 'kata'},
    {label: 'Sysbox', value: 'sysbox'},
  ].filter(({value}) => available.has(value as 'kata' | 'sysbox'));
});
const runtimeOptions = computed(() =>
  cluster.value.runtimeClasses
    .filter(({isolation}) => isolation === form.value?.runtime.isolation)
    .map(({name, handler}) => ({label: name, value: name, handler})),
);
const storageOptions = computed(() =>
  cluster.value.storageClasses.map(({name, provisioner, default: isDefault}) => ({
    label: isDefault ? `${name} (${t('environment.defaultStorage')})` : name,
    value: name,
    provisioner,
  })),
);
const selectIsolation = computed(
  () =>
    isolationOptions.value.length > 1 ||
    !isolationOptions.value.some(({value}) => value === form.value?.runtime.isolation),
);
const selectRuntimeClass = computed(
  () =>
    runtimeOptions.value.length > 1 ||
    !runtimeOptions.value.some(({value}) => value === form.value?.runtime.runtimeClassName),
);
const selectStorageClass = computed(
  () =>
    storageOptions.value.length > 1 ||
    !storageOptions.value.some(({value}) => value === form.value?.runtime.storageClassName),
);

async function load() {
  busy.value = true;
  error.value = '';
  try {
    [items.value, cluster.value] = await Promise.all([
      request<Resource<Environment>[]>('environments'),
      request<ClusterOptions>('cluster-options'),
    ]);
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    busy.value = false;
  }
}
onMounted(load);

function setIsolation(value: string) {
  if (!form.value) return;
  form.value.runtime.isolation = value;
  form.value.runtime.runtimeClassName =
    cluster.value.runtimeClasses.find(({isolation}) => isolation === value)?.name ?? '';
}

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
          isolation: isolationOptions.value[0]?.value ?? 'kata',
          runtimeClassName: '',
          storageClassName: cluster.value.storageClasses[0]?.name ?? '',
          storage: {storage: '20Gi'},
          accessMode: 'ReadWriteOncePod',
          resources: {requests: {cpu: '1', memory: '1Gi'}, limits: {cpu: '2', memory: '2Gi'}},
          gitSSHKeyType: 'ed25519',
          devContainer: {webIDE: {enabled: true, version: '4.121.0', extensions: []}, features: []},
        },
      };
  if (!item) setIsolation(form.value.runtime.isolation);
  extensionsJSON.value = JSON.stringify(form.value.runtime.devContainer.webIDE.extensions ?? [], null, 2);
  featuresJSON.value = JSON.stringify(
    Object.fromEntries(
      (form.value.runtime.devContainer.features ?? []).map(({reference, options}) => [reference, options ?? {}]),
    ),
    null,
    2,
  );
  editing.value = true;
  dirty.beginTracking();
}

function close() {
  if (!dirty.confirmDiscard()) return;
  editing.value = false;
  dirty.endTracking();
}

function parseDevContainerAdditions() {
  const extensions: unknown = JSON.parse(extensionsJSON.value);
  if (
    !Array.isArray(extensions) ||
    extensions.some((value) => typeof value !== 'string' || !value.trim()) ||
    new Set(extensions.map((value) => value.trim())).size !== extensions.length
  ) {
    throw new Error(t('environment.invalidExtensions'));
  }
  const featureObject: unknown = JSON.parse(featuresJSON.value);
  if (!featureObject || Array.isArray(featureObject) || typeof featureObject !== 'object')
    throw new Error(t('environment.invalidFeatures'));
  const features = Object.entries(featureObject).map(([reference, options]) => {
    if (!reference.trim() || !options || Array.isArray(options) || typeof options !== 'object')
      throw new Error(t('environment.invalidFeatures'));
    return {reference: reference.trim(), options: options as Record<string, unknown>};
  });
  return {extensions: extensions.map((value) => value.trim()), features};
}

async function save() {
  busy.value = true;
  error.value = '';
  try {
    if (
      !cluster.value.runtimeClasses.some(
        ({name: runtimeName, isolation}) =>
          runtimeName === form.value!.runtime.runtimeClassName && isolation === form.value!.runtime.isolation,
      )
    )
      throw new Error(t('environment.invalidRuntimeClass'));
    if (
      !cluster.value.storageClasses.some(({name: storageName}) => storageName === form.value!.runtime.storageClassName)
    )
      throw new Error(t('environment.invalidStorageClass'));
    const {extensions, features} = parseDevContainerAdditions();
    form.value!.runtime.devContainer.features = features;
    if (form.value!.runtime.devContainer.webIDE.enabled)
      form.value!.runtime.devContainer.webIDE.extensions = extensions;
    else form.value!.runtime.devContainer.webIDE = {enabled: false, extensions: []};
    await request(`environments${selected.value ? `/${name.value}` : ''}`, selected.value ? 'PUT' : 'POST', {
      name: name.value,
      uid: selected.value?.uid ?? '',
      resourceVersion: selected.value?.resourceVersion ?? '',
      spec: form.value,
      verification: verification.value,
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

function remove(item: Resource<Environment>) {
  dialog.warning({
    title: t('environment.deleteTitle', {name: item.name}),
    content: t('environment.deleteContent'),
    positiveText: t('common.delete'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      try {
        await request(`environments/${item.name}`, 'DELETE', {
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
    <h1>{{ editing ? (selected ? t('environment.edit') : t('environment.new')) : t('environment.title') }}</h1>
    <div
      v-if="!editing"
      class="actions"
    >
      <NButton
        :aria-label="t('environment.refresh')"
        :title="t('environment.refresh')"
        :disabled="busy"
        @click="load"
      >
        <template #icon><RefreshCw :size="16" /></template>
      </NButton>
      <NButton
        type="primary"
        :disabled="!cluster.runtimeClasses.length || !cluster.storageClasses.length"
        @click="edit()"
      >
        {{ t('environment.add') }}
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
  <NAlert
    v-else-if="!cluster.runtimeClasses.length"
    type="warning"
    class="notice"
  >
    {{ t('environment.noRuntimeClasses') }}
  </NAlert>
  <NAlert
    v-else-if="!cluster.storageClasses.length"
    type="warning"
    class="notice"
  >
    {{ t('environment.noStorageClasses') }}
  </NAlert>

  <NForm
    v-if="editing && form"
    class="editor"
    @submit.prevent="save"
  >
    <h2>{{ t('environment.runtimeSection') }}</h2>
    <div class="fields">
      <NFormItem :label="t('common.resourceName')">
        <NInput
          v-model:value="name"
          :disabled="!!selected"
          required
        />
      </NFormItem>
      <NFormItem :label="t('environment.tag')">
        <NInput
          v-model:value="form.tag"
          required
        />
      </NFormItem>
    </div>
    <NFormItem :label="t('environment.description')"><NInput v-model:value="form.description" /></NFormItem>
    <div class="fields">
      <NFormItem :label="t('environment.isolation')">
        <NSelect
          v-if="selectIsolation"
          :value="form.runtime.isolation"
          :options="isolationOptions"
          @update:value="setIsolation"
        />
        <div
          v-else
          class="selection-summary"
        >
          <strong>{{ isolationOptions[0]?.label }}</strong>
        </div>
      </NFormItem>
      <NFormItem :label="t('environment.runtimeClass')">
        <NSelect
          v-if="selectRuntimeClass"
          v-model:value="form.runtime.runtimeClassName"
          :options="runtimeOptions"
        />
        <div
          v-else
          class="selection-summary"
        >
          <strong>{{ runtimeOptions[0]?.label }}</strong>
          <span
            v-if="runtimeOptions[0]?.handler"
            class="muted"
          >
            {{ t('environment.runtimeHandler', {handler: runtimeOptions[0].handler}) }}
          </span>
        </div>
      </NFormItem>
    </div>

    <h2>{{ t('environment.storageSection') }}</h2>
    <div class="fields">
      <NFormItem :label="t('environment.storageClass')">
        <NSelect
          v-if="selectStorageClass"
          v-model:value="form.runtime.storageClassName"
          :options="storageOptions"
        />
        <div
          v-else
          class="selection-summary"
        >
          <strong>{{ storageOptions[0]?.label }}</strong>
          <span
            v-if="storageOptions[0]?.provisioner"
            class="muted"
          >
            {{ storageOptions[0].provisioner }}
          </span>
        </div>
      </NFormItem>
      <NFormItem :label="t('environment.volumeAccess')">
        <NSelect
          v-model:value="form.runtime.accessMode"
          :options="[
            {label: 'ReadWriteOncePod', value: 'ReadWriteOncePod'},
            {label: 'ReadWriteOnce', value: 'ReadWriteOnce'},
          ]"
        />
      </NFormItem>
      <NFormItem :label="t('environment.storage')">
        <NInput
          v-model:value="form.runtime.storage.storage"
          required
        />
      </NFormItem>
      <NFormItem :label="t('environment.sshKeyType')">
        <NSelect
          v-model:value="form.runtime.gitSSHKeyType"
          :options="[
            {label: 'Ed25519', value: 'ed25519'},
            {label: 'RSA 4096', value: 'rsa-4096'},
          ]"
        />
      </NFormItem>
      <NFormItem :label="t('environment.cpuRequest')">
        <NInput
          v-model:value="form.runtime.resources.requests.cpu"
          required
        />
      </NFormItem>
      <NFormItem :label="t('environment.cpuLimit')">
        <NInput
          v-model:value="form.runtime.resources.limits.cpu"
          required
        />
      </NFormItem>
      <NFormItem :label="t('environment.memoryRequest')">
        <NInput
          v-model:value="form.runtime.resources.requests.memory"
          required
        />
      </NFormItem>
      <NFormItem :label="t('environment.memoryLimit')">
        <NInput
          v-model:value="form.runtime.resources.limits.memory"
          required
        />
      </NFormItem>
    </div>

    <h2>{{ t('environment.devContainerSection') }}</h2>
    <NFormItem>
      <NCheckbox v-model:checked="form.runtime.devContainer.webIDE.enabled">
        {{ t('environment.webIDEEnabled') }}
      </NCheckbox>
    </NFormItem>
    <template v-if="form.runtime.devContainer.webIDE.enabled">
      <NFormItem :label="t('environment.webIDEVersion')">
        <NInput
          v-model:value="form.runtime.devContainer.webIDE.version"
          required
        />
      </NFormItem>
      <NFormItem :label="t('environment.extensions')">
        <JsonEditor
          v-model="extensionsJSON"
          :label="t('environment.jsonExtensionsLabel')"
        />
      </NFormItem>
    </template>
    <NFormItem :label="t('environment.features')">
      <JsonEditor
        v-model="featuresJSON"
        :label="t('environment.jsonFeaturesLabel')"
      />
    </NFormItem>
    <NFormItem :label="t('environment.verification')">
      <NInput
        v-model:value="verification"
        :placeholder="t('environment.verificationPlaceholder')"
      />
    </NFormItem>
    <div class="form-actions">
      <NButton
        type="primary"
        attr-type="submit"
        :loading="busy"
      >
        {{ t('environment.save') }}
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
          <th>{{ t('environment.template') }}</th>
          <th>{{ t('environment.runtime') }}</th>
          <th>{{ t('environment.resources') }}</th>
          <th>{{ t('environment.status') }}</th>
          <th>{{ t('common.actions') }}</th>
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
                :aria-label="t('environment.editAction')"
                :title="t('environment.editAction')"
                :disabled="item.deleting"
                @click="edit(item)"
              >
                <template #icon><Pencil :size="16" /></template>
              </NButton>
              <NButton
                :aria-label="t('environment.deleteAction')"
                :title="t('environment.deleteAction')"
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
            {{ busy ? t('environment.loading') : t('environment.empty') }}
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
