<script setup lang="ts">
import {computed} from 'vue';
import {NTag} from 'naive-ui';
import type {Resource} from '../api';

const props = defineProps<{resource: Resource<unknown>}>();
const condition = computed(() =>
  props.resource.status?.conditions?.find((item) => item.type === 'Ready' || item.type === 'InfrastructureReady'),
);
const current = computed(() => condition.value?.observedGeneration === props.resource.generation);
</script>

<template>
  <NTag
    v-if="resource.deleting"
    size="small"
    type="warning"
  >
    Deleting
  </NTag>
  <template v-else-if="condition && current">
    <NTag
      size="small"
      :type="condition.status === 'True' ? 'success' : 'warning'"
    >
      {{ condition.reason }}
    </NTag>
    <p
      v-if="condition.status !== 'True'"
      class="muted"
    >
      {{ condition.message }}
    </p>
  </template>
  <NTag
    v-else
    size="small"
  >
    Pending verification
  </NTag>
</template>
