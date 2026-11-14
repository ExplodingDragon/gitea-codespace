<script setup lang="ts">
import {computed, onMounted, ref, watch} from 'vue';
import {NAlert, NButton, NCheckbox, NForm, NFormItem, NInput, NInputNumber, NSelect, useDialog} from 'naive-ui';
import {Pencil, Trash2, RefreshCw, X} from '@lucide/vue';
import {useI18n} from 'vue-i18n';
import {
  errorMessage,
  request,
  upsertResource,
  type Component,
  type Environment,
  type Resource,
  type Site,
} from '../api';
import {useDirtyForm} from '../composables/useDirtyForm';
import MeasurementInput from '../components/MeasurementInput.vue';
import ResourceStatus from '../components/ResourceStatus.vue';
import SiteNetworkFields from '../components/SiteNetworkFields.vue';
import {cloneFormValue} from '../utils/form';
import {measurementBaseValue, type MeasurementKind} from '../utils/measurement';

const sites = ref<Resource<Site>[]>([]);
const {t} = useI18n();
const environments = ref<Resource<Environment>[]>([]);
const components = ref<Resource<Component>[]>([]);
const editing = ref(false);
const selected = ref<Resource<Site>>();
const form = ref<Site>();
const name = ref('');
const registrationToken = ref('');
const error = ref('');
const loading = ref(false);
const saving = ref(false);
const quotaEnabled = ref(false);
const quotaDraft = ref<Record<string, string>>({});
const dialog = useDialog();
const dirty = useDirtyForm(() => ({
  form: form.value,
  registrationToken: registrationToken.value,
  quotaEnabled: quotaEnabled.value,
  quota: quotaDraft.value,
}));
const siteUsesHTTPS = computed(() => form.value?.url.trim().toLowerCase().startsWith('https://') ?? false);
type QuotaField = {key: string; label: string; kind: MeasurementKind; initial: string; unused?: boolean};
type BaseQuotaField = {key: string; labelKey: string; kind: MeasurementKind; initial: string};
const baseQuotaFields: BaseQuotaField[] = [
  {key: 'pods', labelKey: 'site.pods', kind: 'count', initial: '10'},
  {key: 'persistentvolumeclaims', labelKey: 'site.persistentVolumes', kind: 'count', initial: '10'},
  {key: 'requests.cpu', labelKey: 'site.cpuRequests', kind: 'cpu', initial: '10'},
  {key: 'requests.memory', labelKey: 'site.memoryRequests', kind: 'memory', initial: '10Gi'},
  {key: 'requests.storage', labelKey: 'site.storage', kind: 'storage', initial: '100Gi'},
];
const quotaFields = computed<QuotaField[]>(() => {
  const fields: QuotaField[] = baseQuotaFields.map(({key, labelKey, kind, initial}) => ({
    key,
    label: t(labelKey),
    kind,
    initial,
  }));
  const selectedTemplates = new Set(form.value?.templates.map(({uid}) => uid) ?? []);
  const devices = new Map<string, number>();
  for (const environment of environments.value) {
    if (!selectedTemplates.has(environment.uid)) continue;
    for (const device of environment.spec.runtime.native?.devicePlugins ?? []) {
      devices.set(device.resourceName, Math.max(devices.get(device.resourceName) ?? 0, device.count));
    }
  }
  for (const [resource, count] of [...devices].sort(([left], [right]) => left.localeCompare(right))) {
    fields.push({
      key: `requests.${resource}`,
      label: t('site.deviceQuota', {resource}),
      kind: 'count',
      initial: String(count),
    });
  }
  const known = new Set(fields.map(({key}) => key));
  for (const [key, value] of Object.entries(form.value?.quota ?? {})) {
    if (known.has(key) || !key.startsWith('requests.')) continue;
    const resource = key.slice('requests.'.length);
    fields.push({
      key,
      label: t('site.deviceQuotaUnused', {resource}),
      kind: 'count',
      initial: value,
      unused: true,
    });
  }
  return fields;
});
watch(quotaFields, (fields) => {
  for (const field of fields) quotaDraft.value[field.key] ??= field.initial;
});
function removeUnusedQuota(key: string) {
  delete quotaDraft.value[key];
  if (form.value) delete form.value.quota[key];
}
async function load() {
  loading.value = true;
  error.value = '';
  try {
    [sites.value, environments.value, components.value] = await Promise.all([
      request<Resource<Site>[]>('sites'),
      request<Resource<Environment>[]>('environments'),
      request<Resource<Component>[]>('components'),
    ]);
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    loading.value = false;
  }
}
onMounted(load);
function edit(site?: Resource<Site>) {
  selected.value = site;
  name.value = site?.name ?? '';
  registrationToken.value = '';
  error.value = '';
  form.value = site
    ? cloneFormValue(site.spec)
    : {
        displayName: '',
        url: '',
        caBundle: '',
        enabled: false,
        acceptCreates: true,
        startupConcurrency: 1,
        cleanupConcurrency: 16,
        templates: [],
        gateway: {name: '', uid: ''},
        caches: [],
        quota: {},
        network: {allowSameSite: false, egress: {allowInternet: false, internalCIDRs: [], allow: []}},
      };
  form.value.caches ||= [];
  quotaEnabled.value = Object.keys(form.value.quota ?? {}).length > 0;
  quotaDraft.value = quotaEnabled.value
    ? cloneFormValue(form.value.quota)
    : Object.fromEntries(baseQuotaFields.map(({key, initial}) => [key, initial]));
  editing.value = true;
  dirty.beginTracking();
}
function close() {
  if (!dirty.confirmDiscard()) return;
  editing.value = false;
  registrationToken.value = '';
  dirty.endTracking();
}
async function save() {
  saving.value = true;
  error.value = '';
  try {
    if (quotaEnabled.value) {
      for (const field of quotaFields.value) {
        if ((measurementBaseValue(quotaDraft.value[field.key], field.kind) ?? 0) <= 0)
          throw new Error(t('site.invalidQuota', {resource: field.label}));
      }
    }
    const spec = cloneFormValue(form.value!);
    if (!siteUsesHTTPS.value) spec.caBundle = '';
    const quotaKeys = new Set(quotaFields.value.map(({key}) => key));
    for (const key of Object.keys(spec.quota ?? {})) quotaKeys.add(key);
    spec.quota = quotaEnabled.value
      ? Object.fromEntries(Object.entries(quotaDraft.value).filter(([key]) => quotaKeys.has(key)))
      : {};
    const saved = await request<Resource<Site>>(
      `sites${selected.value ? `/${name.value}` : ''}`,
      selected.value ? 'PUT' : 'POST',
      {
        name: selected.value ? name.value : '',
        uid: selected.value?.uid ?? '',
        resourceVersion: selected.value?.resourceVersion ?? '',
        spec,
        registrationToken: registrationToken.value,
      },
    );
    sites.value = upsertResource(sites.value, saved);
    registrationToken.value = '';
    editing.value = false;
    dirty.endTracking();
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    saving.value = false;
  }
}
function remove(site: Resource<Site>) {
  dialog.warning({
    title: t('site.deleteTitle', {name: site.spec.displayName}),
    content: t('site.deleteContent'),
    positiveText: t('common.delete'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      try {
        await request(`sites/${site.name}`, 'DELETE', {
          name: site.name,
          uid: site.uid,
          resourceVersion: site.resourceVersion,
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
    <h1>{{ editing ? (selected ? t('site.edit') : t('site.new')) : t('site.title') }}</h1>
    <div
      v-if="!editing"
      class="page-actions"
    >
      <NButton
        quaternary
        :aria-label="t('site.refresh')"
        :title="t('site.refresh')"
        :disabled="loading"
        @click="load"
      >
        <template #icon><RefreshCw :size="16" /></template>
      </NButton>
      <NButton
        type="primary"
        @click="edit()"
      >
        {{ t('site.add') }}
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
    <h2>{{ t('site.registrationSection') }}</h2>
    <div class="form-grid">
      <NFormItem :label="t('site.displayName')">
        <NInput
          v-model:value="form.displayName"
          required
        />
      </NFormItem>
      <NFormItem :label="t('site.url')">
        <NInput
          v-model:value="form.url"
          placeholder="https://gitea.example.com"
          required
        />
      </NFormItem>
      <NFormItem
        class="field-wide"
        :label="t('site.caBundle')"
        :feedback="siteUsesHTTPS ? t('site.caBundleHelp') : t('site.caBundleHTTPS')"
      >
        <NInput
          v-model:value="form.caBundle"
          type="textarea"
          :autosize="{minRows: 5, maxRows: 14}"
          :disabled="!siteUsesHTTPS"
          placeholder="-----BEGIN CERTIFICATE-----"
        />
      </NFormItem>
      <NFormItem
        v-if="!selected"
        class="field-wide"
        :label="t('site.registrationToken')"
        :feedback="t('site.registrationTokenHelp')"
      >
        <NInput
          v-model:value="registrationToken"
          type="password"
          autocomplete="new-password"
          required
        />
      </NFormItem>
    </div>
    <h2>{{ t('site.assignmentsSection') }}</h2>
    <p class="section-help">{{ t('site.assignmentsHelp') }}</p>
    <div class="form-grid">
      <NFormItem :label="t('site.templates')">
        <NSelect
          multiple
          :value="form.templates.map((item) => item.uid)"
          :options="
            environments.map((item) => ({
              label: item.spec.description ? `${item.name} — ${item.spec.description}` : item.name,
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
      <NFormItem :label="t('site.gateway')">
        <NSelect
          :value="form.gateway.uid || null"
          :options="
            components
              .filter((item) => item.spec.role === 'gateway')
              .map((item) => ({label: item.spec.displayName, value: item.uid, disabled: item.deleting}))
          "
          @update:value="
            (value: string) => {
              const item = components.find((item) => item.uid === value)!;
              form!.gateway = {name: item.name, uid: item.uid};
            }
          "
        />
      </NFormItem>
      <NFormItem
        class="field-wide"
        :label="t('site.caches')"
      >
        <NSelect
          multiple
          :value="form.caches.map((item) => item.uid)"
          :options="
            components
              .filter((item) => item.spec.role === 'cache')
              .map((item) => ({label: item.spec.displayName, value: item.uid, disabled: item.deleting}))
          "
          @update:value="
            (values: string[]) => {
              form!.caches = components.filter((item) => values.includes(item.uid)).map(({name, uid}) => ({name, uid}));
            }
          "
        />
      </NFormItem>
    </div>
    <h2>{{ t('site.lifecycleSection') }}</h2>
    <div class="form-grid">
      <NFormItem
        class="field-wide"
        :label="t('site.lifecycle')"
        :feedback="t('site.lifecycleHelp')"
      >
        <div class="checkboxes">
          <NCheckbox v-model:checked="form.enabled">{{ t('site.enabled') }}</NCheckbox>
          <NCheckbox v-model:checked="form.acceptCreates">{{ t('site.acceptCreates') }}</NCheckbox>
        </div>
      </NFormItem>
      <NFormItem :label="t('site.startups')">
        <NInputNumber
          v-model:value="form.startupConcurrency"
          :min="1"
          :max="64"
        />
      </NFormItem>
      <NFormItem :label="t('site.cleanups')">
        <NInputNumber
          v-model:value="form.cleanupConcurrency"
          :min="1"
          :max="64"
        />
      </NFormItem>
    </div>
    <h2>{{ t('site.quota') }}</h2>
    <p class="section-help">{{ t('site.quotaHelp') }}</p>
    <NFormItem>
      <NCheckbox v-model:checked="quotaEnabled">{{ t('site.quotaEnabled') }}</NCheckbox>
    </NFormItem>
    <div
      v-if="quotaEnabled"
      class="form-grid quota-fields"
    >
      <NFormItem
        v-for="field in quotaFields"
        :key="field.key"
        :label="field.label"
      >
        <div class="quota-input">
          <MeasurementInput
            v-model="quotaDraft[field.key]"
            :kind="field.kind"
            :aria-label="field.label"
          />
          <NButton
            v-if="field.unused"
            quaternary
            circle
            type="error"
            :aria-label="t('site.removeUnusedDeviceQuota', {resource: field.key})"
            :title="t('site.removeUnusedDeviceQuota', {resource: field.key})"
            @click="removeUnusedQuota(field.key)"
          >
            <template #icon><X :size="16" /></template>
          </NButton>
        </div>
      </NFormItem>
    </div>
    <SiteNetworkFields v-model="form.network" />
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
        {{ t('site.save') }}
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
          <th>{{ t('site.site') }}</th>
          <th>{{ t('site.gitea') }}</th>
          <th>{{ t('site.availability') }}</th>
          <th class="table-actions">{{ t('common.actions') }}</th>
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
          </td>
          <td><ResourceStatus :resource="site" /></td>
          <td class="table-actions">
            <div class="table-action-buttons">
              <NButton
                size="small"
                quaternary
                :aria-label="t('site.editAction')"
                :title="t('site.editAction')"
                :disabled="site.deleting"
                @click="edit(site)"
              >
                <template #icon><Pencil :size="16" /></template>
              </NButton>
              <NButton
                size="small"
                quaternary
                type="error"
                :aria-label="t('site.deleteAction')"
                :title="t('site.deleteAction')"
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
            {{ loading ? t('site.loading') : t('site.empty') }}
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
