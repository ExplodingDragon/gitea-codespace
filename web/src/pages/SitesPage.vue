<script setup lang="ts">
import {computed, onMounted, ref, toRaw} from 'vue';
import {NAlert, NButton, NCheckbox, NForm, NFormItem, NInput, NInputNumber, NSelect, useDialog} from 'naive-ui';
import {Pencil, Trash2, RefreshCw} from '@lucide/vue';
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
const quotaFields = computed<{key: string; label: string; kind: MeasurementKind}[]>(() => [
  {key: 'pods', label: t('site.pods'), kind: 'count'},
  {key: 'persistentvolumeclaims', label: t('site.persistentVolumes'), kind: 'count'},
  {key: 'requests.cpu', label: t('site.cpuRequests'), kind: 'cpu'},
  {key: 'requests.memory', label: t('site.memoryRequests'), kind: 'memory'},
  {key: 'requests.storage', label: t('site.storage'), kind: 'storage'},
]);
function defaultQuota() {
  return {
    pods: '10',
    persistentvolumeclaims: '10',
    'requests.cpu': '10',
    'requests.memory': '10Gi',
    'requests.storage': '100Gi',
  };
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
    ? structuredClone(toRaw(site.spec))
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
        upstreams: [],
      };
  form.value.caches ||= [];
  form.value.upstreams ||= [];
  quotaEnabled.value = Object.keys(form.value.quota ?? {}).length > 0;
  quotaDraft.value = quotaEnabled.value ? structuredClone(toRaw(form.value.quota)) : defaultQuota();
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
    const spec = structuredClone(toRaw(form.value!));
    if (!siteUsesHTTPS.value) spec.caBundle = '';
    spec.quota = quotaEnabled.value ? structuredClone(toRaw(quotaDraft.value)) : {};
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
        <MeasurementInput
          v-model="quotaDraft[field.key]"
          :kind="field.kind"
          :aria-label="field.label"
        />
      </NFormItem>
    </div>
    <h2>{{ t('site.upstreams') }}</h2>
    <p class="section-help">{{ t('site.upstreamsHelp') }}</p>
    <div
      v-for="(upstream, index) in form.upstreams"
      :key="index"
      class="upstream-row"
    >
      <NFormItem :label="t('site.cidr')">
        <NInput
          v-model:value="upstream.cidr"
          placeholder="10.0.0.10/32"
        />
      </NFormItem>
      <NFormItem :label="t('site.ports')">
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
        type="error"
        quaternary
        :aria-label="t('site.removeUpstream')"
        :title="t('site.removeUpstream')"
        @click="form.upstreams.splice(index, 1)"
      >
        <template #icon><Trash2 :size="16" /></template>
      </NButton>
    </div>
    <NButton
      class="add-row"
      @click="form.upstreams.push({cidr: '', ports: [443]})"
    >
      {{ t('site.addUpstream') }}
    </NButton>
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
