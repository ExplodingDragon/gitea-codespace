<script setup lang="ts">
import {computed} from 'vue';
import {NCollapse, NCollapseItem, NTag} from 'naive-ui';
import {useI18n} from 'vue-i18n';
import type {Resource} from '../api';

const props = defineProps<{resource: Resource<unknown>}>();
const {t, te} = useI18n();
const condition = computed(() =>
  props.resource.status?.conditions?.find((item) => item.type === 'Ready' || item.type === 'InfrastructureReady'),
);
const current = computed(() => condition.value?.observedGeneration === props.resource.generation);
const conditionSummary = computed(() => {
  const key = `conditions.${condition.value?.reason}`;
  return condition.value && te(key) ? t(key) : condition.value?.message;
});
</script>

<template>
  <NTag
    v-if="resource.deleting"
    size="small"
    type="warning"
  >
    {{ t('common.deleting') }}
  </NTag>
  <template v-else-if="condition && current">
    <NTag
      size="small"
      :type="condition.status === 'True' ? 'success' : 'warning'"
    >
      {{ condition.status === 'True' ? t('common.ready') : t('common.notReady') }}
    </NTag>
    <p
      v-if="condition.status !== 'True'"
      class="muted"
    >
      {{ conditionSummary }}
    </p>
    <NCollapse
      v-if="condition.status !== 'True' && condition.message && condition.message !== conditionSummary"
      class="technical-details"
    >
      <NCollapseItem :title="t('common.technicalDetails')">
        <code>{{ condition.message }}</code>
      </NCollapseItem>
    </NCollapse>
  </template>
  <NTag
    v-else
    size="small"
  >
    {{ t('common.pendingVerification') }}
  </NTag>
</template>
