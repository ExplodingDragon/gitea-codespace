<script setup lang="ts">
import {computed, onMounted, ref, toRaw} from 'vue';
import {NAlert, NButton, NCheckbox, NForm, NFormItem, NInput, NInputNumber, NSelect, useDialog} from 'naive-ui';
import {Pencil, Trash2, RefreshCw} from '@lucide/vue';
import {useI18n} from 'vue-i18n';
import {errorMessage, request, type Component, type Environment, type Resource, type Site} from '../api';
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
const busy = ref(false);
const dialog = useDialog();
const dirty = useDirtyForm(() => ({form: form.value, registrationToken: registrationToken.value}));
const quotaFields = computed<{key: string; label: string; kind: MeasurementKind}[]>(() => [
  {key: 'pods', label: t('site.pods'), kind: 'count'},
  {key: 'persistentvolumeclaims', label: t('site.persistentVolumes'), kind: 'count'},
  {key: 'requests.cpu', label: t('site.cpuRequests'), kind: 'cpu'},
  {key: 'limits.cpu', label: t('site.cpuLimits'), kind: 'cpu'},
  {key: 'requests.memory', label: t('site.memoryRequests'), kind: 'memory'},
  {key: 'limits.memory', label: t('site.memoryLimits'), kind: 'memory'},
  {key: 'requests.storage', label: t('site.storage'), kind: 'storage'},
]);
async function load() {
  busy.value = true;
  error.value = '';
  try {
    sites.value = await request('sites');
    environments.value = await request('environments');
    components.value = await request('components');
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    busy.value = false;
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
  registrationToken.value = '';
  dirty.endTracking();
}
async function save() {
  busy.value = true;
  error.value = '';
  try {
    for (const field of quotaFields.value) {
      if ((measurementBaseValue(form.value!.quota[field.key], field.kind) ?? 0) <= 0)
        throw new Error(t('site.invalidQuota', {resource: field.label}));
    }
    await request(`sites${selected.value ? `/${name.value}` : ''}`, selected.value ? 'PUT' : 'POST', {
      name: selected.value ? name.value : '',
      uid: selected.value?.uid ?? '',
      resourceVersion: selected.value?.resourceVersion ?? '',
      spec: form.value,
      registrationToken: registrationToken.value,
    });
    registrationToken.value = '';
    editing.value = false;
    dirty.endTracking();
    await load();
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    busy.value = false;
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
      class="actions"
    >
      <NButton
        :aria-label="t('site.refresh')"
        :title="t('site.refresh')"
        :disabled="busy"
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
    <div class="fields">
      <NFormItem
        v-if="selected"
        :label="t('common.resourceName')"
      >
        <NInput
          v-model:value="name"
          disabled
        />
      </NFormItem>
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
    </div>
    <NFormItem
      v-if="!selected"
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
    <h2>{{ t('site.assignmentsSection') }}</h2>
    <p class="section-help">{{ t('site.assignmentsHelp') }}</p>
    <div class="fields">
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
      <NFormItem :label="t('site.caches')">
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
    <div class="fields">
      <NFormItem
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
    <div class="fields quota-fields">
      <NFormItem
        v-for="field in quotaFields"
        :key="field.key"
        :label="field.label"
      >
        <MeasurementInput
          v-model="form.quota[field.key]"
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
        quaternary
        :aria-label="t('site.removeUpstream')"
        :title="t('site.removeUpstream')"
        @click="form.upstreams.splice(index, 1)"
      >
        <template #icon><Trash2 :size="16" /></template>
      </NButton>
    </div>
    <NButton @click="form.upstreams.push({cidr: '', ports: [443]})">{{ t('site.addUpstream') }}</NButton>
    <div class="form-actions">
      <NButton
        type="primary"
        attr-type="submit"
        :loading="busy"
      >
        {{ t('site.save') }}
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
          <th>{{ t('site.site') }}</th>
          <th>{{ t('site.gitea') }}</th>
          <th>{{ t('site.availability') }}</th>
          <th>{{ t('common.actions') }}</th>
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
          <td>
            <div class="actions">
              <NButton
                :aria-label="t('site.editAction')"
                :title="t('site.editAction')"
                :disabled="site.deleting"
                @click="edit(site)"
              >
                <template #icon><Pencil :size="16" /></template>
              </NButton>
              <NButton
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
            {{ busy ? t('site.loading') : t('site.empty') }}
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
