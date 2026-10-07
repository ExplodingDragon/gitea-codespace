<script setup lang="ts">
import {onMounted, ref} from 'vue';
import {NAlert, NButton, NTag, useDialog} from 'naive-ui';
import {RefreshCw} from '@lucide/vue';
import {request, type RuntimeInstance, type Resource} from '../api';

const items = ref<Resource<RuntimeInstance>[]>([]);
const error = ref('');
const busy = ref(false);
const dialog = useDialog();

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
    title: 'Confirm runtime recovery',
    content:
      'Confirm only after the previous runtime Pod can no longer write to its volume. The runtime will converge to stopped before it can be resumed.',
    positiveText: 'Confirm stopped',
    negativeText: 'Cancel',
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
        error.value = (e as Error).message;
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
    error.value = (e as Error).message;
  } finally {
    busy.value = false;
  }
}
onMounted(load);
</script>

<template>
  <div class="page-heading">
    <h1>Running environments</h1>
    <NButton
      aria-label="Refresh environments"
      title="Refresh environments"
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
    <table>
      <thead>
        <tr>
          <th>Codespace</th>
          <th>Site</th>
          <th>Environment</th>
          <th>Operation</th>
          <th>Runtime</th>
          <th class="runtime-actions">Actions</th>
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
              size="small"
              :type="item.status?.ready ? 'success' : 'default'"
            >
              {{ item.deleting ? 'Deleting' : item.status?.ready ? 'Ready' : 'Not ready' }}
            </NTag>
            <p class="muted">{{ item.status?.pod?.name }}</p>
          </td>
          <td class="runtime-actions">
            <NButton
              v-if="recoveryRequired(item)"
              size="small"
              :disabled="busy"
              @click="confirmWriterStopped(item)"
            >
              Confirm stopped
            </NButton>
          </td>
        </tr>
        <tr v-if="!items.length">
          <td
            colspan="6"
            class="empty"
          >
            {{ busy ? 'Loading environments...' : 'No running environments' }}
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
