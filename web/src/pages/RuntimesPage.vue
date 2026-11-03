<script setup lang="ts">
import {onMounted, ref} from 'vue';
import {NAlert, NButton, useDialog} from 'naive-ui';
import {RefreshCw} from '@lucide/vue';
import {useI18n} from 'vue-i18n';
import {errorMessage, request, type RuntimeInstance, type Resource} from '../api';
import ResourceStatus from '../components/ResourceStatus.vue';

const items = ref<Resource<RuntimeInstance>[]>([]);
const {t} = useI18n();
const error = ref('');
const busy = ref(false);
const dialog = useDialog();

function deleteRuntime(item: Resource<RuntimeInstance>) {
  dialog.warning({
    title: t('runtime.deleteTitle'),
    content: t('runtime.deleteContent'),
    positiveText: t('common.delete'),
    negativeText: t('common.cancel'),
    async onPositiveClick() {
      busy.value = true;
      error.value = '';
      try {
        await request(
          `runtimes/${encodeURIComponent(item.namespace ?? '')}/${encodeURIComponent(item.name)}`,
          'DELETE',
          {
            uid: item.uid,
            resourceVersion: item.resourceVersion,
            podUID: item.status?.pod?.uid,
          },
        );
        await load();
      } catch (e) {
        error.value = errorMessage(e, t);
      } finally {
        busy.value = false;
      }
    },
  });
}

function recoveryRequired(item: Resource<RuntimeInstance>) {
  return item.status?.conditions?.some(
    (condition) =>
      condition.type === 'InfrastructureReady' &&
      condition.status === 'False' &&
      (condition.reason === 'RecoveryRequired' || condition.reason === 'WriterUnconfirmed'),
  );
}

function confirmWriterStopped(item: Resource<RuntimeInstance>) {
  dialog.warning({
    title: t('runtime.recoveryTitle'),
    content: t('runtime.recoveryContent'),
    positiveText: t('runtime.confirmStopped'),
    negativeText: t('common.cancel'),
    async onPositiveClick() {
      busy.value = true;
      error.value = '';
      try {
        await request(
          `runtimes/${encodeURIComponent(item.namespace ?? '')}/${encodeURIComponent(item.name)}/confirm-writer-stopped`,
          'POST',
          {uid: item.uid, resourceVersion: item.resourceVersion, podUID: item.status?.pod?.uid},
        );
        await load();
      } catch (e) {
        error.value = errorMessage(e, t);
      } finally {
        busy.value = false;
      }
    },
  });
}
async function load() {
  busy.value = true;
  error.value = '';
  try {
    items.value = await request('runtimes');
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    busy.value = false;
  }
}
onMounted(load);
</script>

<template>
  <div class="page-heading">
    <h1>{{ t('runtime.title') }}</h1>
    <NButton
      :aria-label="t('runtime.refresh')"
      :title="t('runtime.refresh')"
      :disabled="busy"
      @click="load"
    >
      <template #icon><RefreshCw :size="16" /></template>
    </NButton>
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
          <th class="runtime-actions">{{ t('common.actions') }}</th>
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
            <ResourceStatus :resource="item" />
            <p class="muted">{{ item.status?.pod?.name }}</p>
          </td>
          <td class="runtime-actions">
            <div class="actions">
              <NButton
                v-if="recoveryRequired(item)"
                size="small"
                :disabled="busy"
                @click="confirmWriterStopped(item)"
              >
                {{ t('runtime.confirmStopped') }}
              </NButton>
              <NButton
                size="small"
                type="error"
                :disabled="busy || item.deleting"
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
