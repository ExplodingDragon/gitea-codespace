<script setup lang="ts">
import {computed, onMounted, ref, toRaw} from 'vue';
import {NAlert, NButton, NCheckbox, NForm, NFormItem, NInput, NSelect, NTag, useDialog} from 'naive-ui';
import {Pencil, RefreshCw, Trash2} from '@lucide/vue';
import {useI18n} from 'vue-i18n';
import {errorMessage, request, upsertResource, type ClusterOptions, type Environment, type Resource} from '../api';
import {useDirtyForm} from '../composables/useDirtyForm';
import JsonEditor from '../components/JsonEditor.vue';
import MeasurementInput from '../components/MeasurementInput.vue';
import ResourceStatus from '../components/ResourceStatus.vue';
import {measurementBaseValue} from '../utils/measurement';

const {t} = useI18n();
const items = ref<Resource<Environment>[]>([]);
const cluster = ref<ClusterOptions>({
  runtimeClasses: [],
  storageClasses: [],
  ingressClasses: [],
  tlsSecrets: [],
  issuers: [],
});
const editing = ref(false);
const selected = ref<Resource<Environment>>();
const form = ref<Environment>();
const name = ref('');
const verification = ref('');
const extensionsJSON = ref('[]');
const featuresJSON = ref('{}');
const error = ref('');
const loading = ref(false);
const saving = ref(false);
const limitsEnabled = ref(false);
const limitsDraft = ref({cpu: '1', memory: '1Gi'});
const dialog = useDialog();
const dirty = useDirtyForm(() => ({
  form: form.value,
  verification: verification.value,
  extensions: extensionsJSON.value,
  features: featuresJSON.value,
  limitsEnabled: limitsEnabled.value,
  limits: limitsDraft.value,
}));

const isolationOptions = computed(() => {
  const available = new Set(cluster.value.runtimeClasses.map(({isolation}) => isolation));
  const options = [
    {label: 'Kata', value: 'kata', disabled: false},
    {label: 'Sysbox', value: 'sysbox', disabled: false},
  ].filter(({value}) => available.has(value as 'kata' | 'sysbox'));
  const current = form.value?.runtime.isolation;
  if (current && !options.some(({value}) => value === current))
    options.push({label: t('common.unavailableOption', {name: current}), value: current, disabled: true});
  return options;
});
const runtimeOptions = computed(() => {
  const options = cluster.value.runtimeClasses
    .filter(({isolation}) => isolation === form.value?.runtime.isolation)
    .map(({name, handler}) => ({label: name, value: name, handler, disabled: false}));
  const current = form.value?.runtime.runtimeClassName;
  if (current && !options.some(({value}) => value === current))
    options.push({
      label: t('common.unavailableOption', {name: current}),
      value: current,
      handler: '',
      disabled: true,
    });
  return options;
});
const storageOptions = computed(() => {
  const options = cluster.value.storageClasses.map(({name, provisioner, default: isDefault}) => ({
    label: isDefault ? `${name} (${t('environment.defaultStorage')})` : name,
    value: name,
    provisioner,
    disabled: false,
  }));
  const current = form.value?.runtime.storageClassName;
  if (current && !options.some(({value}) => value === current))
    options.push({
      label: t('common.unavailableOption', {name: current}),
      value: current,
      provisioner: '',
      disabled: true,
    });
  return options;
});

async function load() {
  loading.value = true;
  error.value = '';
  try {
    [items.value, cluster.value] = await Promise.all([
      request<Resource<Environment>[]>('environments'),
      request<ClusterOptions>('cluster-options'),
    ]);
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    loading.value = false;
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
  const requests = item?.spec.runtime.resources.requests;
  const limits = item?.spec.runtime.resources.limits;
  if (item && (!requests?.cpu || !requests.memory || Boolean(limits?.cpu) !== Boolean(limits?.memory))) {
    error.value = t('environment.invalidResources');
    return;
  }
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
          storageClassName:
            cluster.value.storageClasses.find(({default: isDefault}) => isDefault)?.name ??
            (cluster.value.storageClasses.length === 1 ? cluster.value.storageClasses[0].name : ''),
          storage: {storage: '20Gi'},
          accessMode: 'ReadWriteOncePod',
          resources: {requests: {cpu: '100m', memory: '256Mi'}},
          gitSSHKeyType: 'ed25519',
          devContainer: {webIDE: {enabled: true, version: '4.121.0', extensions: []}, features: []},
        },
      };
  if (!item) setIsolation(form.value.runtime.isolation);
  const currentLimits = form.value.runtime.resources.limits;
  if (currentLimits?.cpu && currentLimits.memory) {
    limitsEnabled.value = true;
    limitsDraft.value = {cpu: currentLimits.cpu, memory: currentLimits.memory};
  } else {
    limitsEnabled.value = false;
    limitsDraft.value = {cpu: '1', memory: '1Gi'};
  }
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
  saving.value = true;
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
    const spec = structuredClone(toRaw(form.value!));
    for (const kind of ['cpu', 'memory'] as const) {
      const requestValue = measurementBaseValue(spec.runtime.resources.requests[kind], kind);
      const limit = limitsEnabled.value ? measurementBaseValue(limitsDraft.value[kind], kind) : null;
      if (
        requestValue === null ||
        requestValue <= 0 ||
        (limitsEnabled.value && (limit === null || requestValue > limit))
      )
        throw new Error(t('environment.invalidResourceRange', {resource: t(`units.${kind}`)}));
    }
    if (limitsEnabled.value) spec.runtime.resources.limits = structuredClone(toRaw(limitsDraft.value));
    else delete spec.runtime.resources.limits;
    if ((measurementBaseValue(spec.runtime.storage.storage, 'storage') ?? 0) <= 0)
      throw new Error(t('environment.invalidStorageSize'));
    const {extensions, features} = parseDevContainerAdditions();
    spec.runtime.devContainer.features = features;
    if (spec.runtime.devContainer.webIDE.enabled) spec.runtime.devContainer.webIDE.extensions = extensions;
    else spec.runtime.devContainer.webIDE = {enabled: false, extensions: []};
    const saved = await request<Resource<Environment>>(
      `environments${selected.value ? `/${name.value}` : ''}`,
      selected.value ? 'PUT' : 'POST',
      {
        name: selected.value ? name.value : '',
        uid: selected.value?.uid ?? '',
        resourceVersion: selected.value?.resourceVersion ?? '',
        spec,
        verification: verification.value,
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
        :disabled="loading"
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
    <h2>{{ t('environment.basicSection') }}</h2>
    <div class="fields">
      <NFormItem :label="t('environment.tag')">
        <NInput
          v-model:value="form.tag"
          required
        />
      </NFormItem>
      <NFormItem :label="t('environment.description')"><NInput v-model:value="form.description" /></NFormItem>
    </div>
    <h2>{{ t('environment.runtimeSection') }}</h2>
    <div class="fields">
      <NFormItem
        :label="t('environment.isolation')"
        :feedback="t('environment.isolationHelp')"
      >
        <NSelect
          :value="form.runtime.isolation"
          :options="isolationOptions"
          @update:value="setIsolation"
        />
      </NFormItem>
      <NFormItem
        :label="t('environment.runtimeClass')"
        :feedback="t('environment.runtimeClassHelp')"
      >
        <NSelect
          v-model:value="form.runtime.runtimeClassName"
          :options="runtimeOptions"
        />
      </NFormItem>
    </div>

    <h2>{{ t('environment.storageSection') }}</h2>
    <div class="fields">
      <NFormItem
        :label="t('environment.storageClass')"
        :feedback="t('environment.storageClassHelp')"
      >
        <NSelect
          v-model:value="form.runtime.storageClassName"
          :options="storageOptions"
        />
      </NFormItem>
      <NFormItem
        :label="t('environment.volumeAccess')"
        :feedback="t('environment.volumeAccessHelp')"
      >
        <NSelect
          v-model:value="form.runtime.accessMode"
          :options="[
            {label: 'ReadWriteOncePod', value: 'ReadWriteOncePod'},
            {label: 'ReadWriteOnce', value: 'ReadWriteOnce'},
          ]"
        />
      </NFormItem>
      <NFormItem :label="t('environment.storage')">
        <MeasurementInput
          v-model="form.runtime.storage.storage"
          kind="storage"
          :aria-label="t('environment.storage')"
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
    </div>

    <h2>{{ t('environment.computeSection') }}</h2>
    <p class="section-help">{{ t('environment.computeHelp') }}</p>
    <div class="fields">
      <NFormItem :label="t('environment.cpuRequest')">
        <MeasurementInput
          v-model="form.runtime.resources.requests.cpu"
          kind="cpu"
          :aria-label="t('environment.cpuRequest')"
        />
      </NFormItem>
      <NFormItem :label="t('environment.memoryRequest')">
        <MeasurementInput
          v-model="form.runtime.resources.requests.memory"
          kind="memory"
          :aria-label="t('environment.memoryRequest')"
        />
      </NFormItem>
    </div>
    <NFormItem>
      <NCheckbox v-model:checked="limitsEnabled">{{ t('environment.limitsEnabled') }}</NCheckbox>
    </NFormItem>
    <div
      v-if="limitsEnabled"
      class="fields"
    >
      <NFormItem :label="t('environment.cpuLimit')">
        <MeasurementInput
          v-model="limitsDraft.cpu"
          kind="cpu"
          :aria-label="t('environment.cpuLimit')"
        />
      </NFormItem>
      <NFormItem :label="t('environment.memoryLimit')">
        <MeasurementInput
          v-model="limitsDraft.memory"
          kind="memory"
          :aria-label="t('environment.memoryLimit')"
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
        :loading="saving"
      >
        {{ t('environment.save') }}
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
            <template v-if="item.spec.runtime.resources.requests?.cpu && item.spec.runtime.resources.requests.memory">
              {{ item.spec.runtime.resources.requests.cpu }} CPU / {{ item.spec.runtime.resources.requests.memory }}
              <p
                v-if="item.spec.runtime.resources.limits?.cpu && item.spec.runtime.resources.limits?.memory"
                class="muted"
              >
                {{
                  t('environment.resourceLimits', {
                    cpu: item.spec.runtime.resources.limits?.cpu,
                    memory: item.spec.runtime.resources.limits?.memory,
                  })
                }}
              </p>
              <p
                v-else-if="!item.spec.runtime.resources.limits?.cpu && !item.spec.runtime.resources.limits?.memory"
                class="muted"
              >
                {{ t('environment.noResourceLimits') }}
              </p>
              <NTag
                v-else
                size="small"
                type="error"
              >
                {{ t('environment.invalidResources') }}
              </NTag>
            </template>
            <NTag
              v-else
              size="small"
              type="error"
            >
              {{ t('environment.invalidResources') }}
            </NTag>
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
            {{ loading ? t('environment.loading') : t('environment.empty') }}
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
