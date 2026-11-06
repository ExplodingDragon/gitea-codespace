<script setup lang="ts">
import {onBeforeUnmount, onMounted, ref} from 'vue';
import {NAlert, NButton, NTag, useDialog} from 'naive-ui';
import {RefreshCw} from '@lucide/vue';
import {useI18n} from 'vue-i18n';
import {errorMessage, request, type RuntimeInstance, type RuntimeStatus, type Resource} from '../api';
import ResourceStatus from '../components/ResourceStatus.vue';

const items = ref<Resource<RuntimeInstance, RuntimeStatus>[]>([]);
const {t} = useI18n();
const error = ref('');
const busy = ref(false);
const dialog = useDialog();
let refreshTimer: number | undefined;

async function updateRuntime(path: string, method: 'DELETE' | 'POST', body: unknown) {
  busy.value = true;
  error.value = '';
  try {
    await request(path, method, body);
    await load();
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    busy.value = false;
  }
}

function deleteRuntime(item: Resource<RuntimeInstance, RuntimeStatus>) {
  dialog.warning({
    title: t('runtime.deleteTitle'),
    content: t('runtime.deleteContent'),
    positiveText: t('common.delete'),
    negativeText: t('common.cancel'),
    async onPositiveClick() {
      await updateRuntime(
        `runtimes/${encodeURIComponent(item.namespace ?? '')}/${encodeURIComponent(item.name)}`,
        'DELETE',
        {
          uid: item.uid,
          resourceVersion: item.resourceVersion,
          podUID: item.status?.pod?.uid,
        },
      );
    },
  });
}

async function stopRuntime(item: Resource<RuntimeInstance, RuntimeStatus>) {
  await updateRuntime(
    `runtimes/${encodeURIComponent(item.namespace ?? '')}/${encodeURIComponent(item.name)}/stop`,
    'POST',
    {uid: item.uid, podUID: item.status?.pod?.uid},
  );
}

function hasCurrentInfrastructureProblem(item: Resource<RuntimeInstance, RuntimeStatus>) {
  return item.status?.conditions?.some(
    (condition) =>
      condition.type === 'InfrastructureReady' &&
      condition.status !== 'True' &&
      condition.observedGeneration === item.generation,
  );
}

async function load(background = false) {
  window.clearTimeout(refreshTimer);
  if (!background) {
    busy.value = true;
    error.value = '';
  }
  try {
    items.value = await request('runtimes');
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    if (!background) busy.value = false;
  }
  if (items.value.some((item) => item.status?.stopping || item.status?.deleting || item.deleting)) {
    refreshTimer = window.setTimeout(() => load(true), 1000);
  }
}
onMounted(() => load());
onBeforeUnmount(() => window.clearTimeout(refreshTimer));
</script>

<template>
  <div class="page-heading">
    <h1>{{ t('runtime.title') }}</h1>
    <div class="page-actions">
      <NButton
        quaternary
        :aria-label="t('runtime.refresh')"
        :title="t('runtime.refresh')"
        :disabled="busy"
        @click="load()"
      >
        <template #icon><RefreshCw :size="16" /></template>
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
  <div class="table-scroll">
    <table class="runtime-table">
      <thead>
        <tr>
          <th>{{ t('runtime.codespace') }}</th>
          <th>{{ t('runtime.site') }}</th>
          <th>{{ t('runtime.environment') }}</th>
          <th>{{ t('runtime.operation') }}</th>
          <th>{{ t('runtime.runtime') }}</th>
          <th class="table-actions">{{ t('common.actions') }}</th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="item in items"
          :key="item.uid"
        >
          <td>
            {{ item.spec.codespaceID }}
            <p class="muted">{{ item.name }}</p>
          </td>
          <td>{{ item.spec.site.name }}</td>
          <td>{{ item.spec.environmentTag }}</td>
          <td>{{ item.spec.operation }} #{{ item.spec.version }}</td>
          <td>
            <NTag
              v-if="item.status?.deleting || item.deleting"
              size="small"
              type="warning"
            >
              {{ t('common.deleting') }}
            </NTag>
            <NTag
              v-else-if="item.status?.stopping"
              size="small"
              type="warning"
            >
              {{ t('runtime.stopping') }}
            </NTag>
            <NTag
              v-else-if="item.status?.stopped"
              size="small"
            >
              {{ t('runtime.stopped') }}
            </NTag>
            <NTag
              v-else-if="item.status?.ready"
              size="small"
              type="success"
            >
              {{ t('common.ready') }}
            </NTag>
            <ResourceStatus
              v-else-if="hasCurrentInfrastructureProblem(item)"
              :resource="item"
            />
            <NTag
              v-else
              size="small"
            >
              {{ t('runtime.starting') }}
            </NTag>
            <p
              v-if="item.status?.pod?.name"
              class="muted"
            >
              {{ item.status.pod.name }}
            </p>
          </td>
          <td class="table-actions">
            <div class="table-action-buttons">
              <NButton
                v-if="item.status?.canStop || item.status?.stopping"
                size="small"
                :disabled="busy || item.status?.stopping"
                @click="stopRuntime(item)"
              >
                {{ t(item.status?.stopping ? 'runtime.stopping' : 'runtime.stop') }}
              </NButton>
              <NButton
                size="small"
                type="error"
                :disabled="busy || item.deleting || item.status?.deleting"
                @click="deleteRuntime(item)"
              >
                {{ t('runtime.deleteData') }}
              </NButton>
            </div>
          </td>
        </tr>
        <tr v-if="!items.length">
          <td
            colspan="6"
            class="empty"
          >
            {{ busy ? t('runtime.loading') : t('runtime.empty') }}
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
