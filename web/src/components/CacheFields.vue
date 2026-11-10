<script setup lang="ts">
import {computed} from 'vue';
import {NButton, NCheckbox, NFormItem, NInput, NSelect} from 'naive-ui';
import {Plus, X} from '@lucide/vue';
import {useI18n} from 'vue-i18n';
import type {ClusterOptions, Component, ComponentStatus} from '../api';
import MeasurementInput from './MeasurementInput.vue';
import ProxySettings from './ProxySettings.vue';

const props = defineProps<{
  cache: NonNullable<Component['cache']>;
  cluster: ClusterOptions;
  status?: ComponentStatus;
}>();
const s3AccessKey = defineModel<string>('s3AccessKey', {required: true});
const s3SecretKey = defineModel<string>('s3SecretKey', {required: true});
const upstreams = defineModel<{host: string; allow: string}[]>('upstreams', {required: true});
const {t} = useI18n();
const currentStorageClass = computed(() =>
  props.status?.storageClassLocked
    ? (props.status.storageClassName ?? '')
    : (props.cache.storage.storageClassName ?? ''),
);

const storageOptions = computed(() => {
  const hasDefaultStorage = props.cluster.storageClasses.some(({default: isDefault}) => isDefault);
  const options = [
    {
      label: hasDefaultStorage
        ? t('component.clusterDefaultStorage')
        : t('common.unavailableOption', {name: t('component.clusterDefaultStorage')}),
      value: '__default__',
      disabled: !hasDefaultStorage,
    },
    ...props.cluster.storageClasses.map(({name, default: isDefault}) => ({
      label: isDefault ? `${name} (${t('environment.defaultStorage')})` : name,
      value: name,
      disabled: false,
    })),
  ];
  const current = currentStorageClass.value;
  if (current && !options.some(({value}) => value === current))
    options.push({label: t('common.unavailableOption', {name: current}), value: current, disabled: true});
  return options;
});
</script>

<template>
  <h2>{{ t('component.cacheSection') }}</h2>
  <div class="form-grid">
    <NFormItem class="field-wide">
      <NCheckbox v-model:checked="cache.enabled">{{ t('component.acceptCache') }}</NCheckbox>
    </NFormItem>
    <NFormItem :label="t('component.storageDriver')">
      <NSelect
        v-model:value="cache.storage.driver"
        :options="[
          {label: t('component.persistentVolume'), value: 'filesystem'},
          {label: t('component.s3Storage'), value: 's3'},
        ]"
      />
    </NFormItem>
    <NFormItem
      :label="t('component.maxCacheSize')"
      :feedback="t('component.maxCacheSizeHelp')"
    >
      <MeasurementInput
        v-model="cache.maxSize"
        kind="bytes"
        :aria-label="t('component.maxCacheSize')"
        :allow-empty="cache.storage.driver === 's3'"
      />
    </NFormItem>
    <NFormItem :label="t('component.maxAge')">
      <MeasurementInput
        v-model="cache.maxAge"
        kind="duration"
        :aria-label="t('component.maxAge')"
      />
    </NFormItem>
    <NFormItem :label="t('component.cleanupInterval')">
      <MeasurementInput
        v-model="cache.gcInterval"
        kind="duration"
        :aria-label="t('component.cleanupInterval')"
      />
    </NFormItem>
  </div>
  <div
    v-if="cache.storage.driver === 'filesystem'"
    class="form-grid"
  >
    <NFormItem
      :label="t('component.volumeSize')"
      :feedback="t('component.volumeSizeHelp')"
    >
      <MeasurementInput
        v-model="cache.storage.volumeSize"
        kind="storage"
        :aria-label="t('component.volumeSize')"
      />
    </NFormItem>
    <NFormItem
      :label="t('component.storageClass')"
      :feedback="
        status?.storageClassLocked
          ? t('component.storageClassLockedHelp', {name: currentStorageClass})
          : t('component.storageClassHelp')
      "
    >
      <NSelect
        :value="currentStorageClass || '__default__'"
        :options="storageOptions"
        :disabled="props.status?.storageClassLocked"
        @update:value="cache.storage.storageClassName = $event === '__default__' ? '' : $event"
      />
    </NFormItem>
    <NFormItem :label="t('component.minFreeSpace')">
      <MeasurementInput
        v-model="cache.storage.minFreeSpace"
        kind="bytes"
        :aria-label="t('component.minFreeSpace')"
      />
    </NFormItem>
  </div>
  <div
    v-else
    class="form-grid"
  >
    <NFormItem :label="t('component.s3Endpoint')"><NInput v-model:value="cache.storage.s3.endpoint" /></NFormItem>
    <NFormItem :label="t('component.region')">
      <NInput
        v-model:value="cache.storage.s3.region"
        required
      />
    </NFormItem>
    <NFormItem :label="t('component.bucket')">
      <NInput
        v-model:value="cache.storage.s3.bucket"
        required
      />
    </NFormItem>
    <NFormItem :label="t('component.prefix')"><NInput v-model:value="cache.storage.s3.prefix" /></NFormItem>
    <NFormItem :label="t('component.replaceAccessKey')">
      <NInput
        v-model:value="s3AccessKey"
        autocomplete="off"
      />
    </NFormItem>
    <NFormItem :label="t('component.replaceSecretKey')">
      <NInput
        v-model:value="s3SecretKey"
        type="password"
        autocomplete="new-password"
      />
    </NFormItem>
    <NFormItem>
      <NCheckbox v-model:checked="cache.storage.s3.forcePathStyle">{{ t('component.pathStyle') }}</NCheckbox>
    </NFormItem>
  </div>
  <h2>{{ t('component.mirrors') }}</h2>
  <p class="section-help">{{ t('component.mirrorHelp') }}</p>
  <div
    v-for="(upstream, index) in upstreams"
    :key="index"
    class="upstream-row"
  >
    <NInput
      v-model:value="upstream.host"
      placeholder="ghcr.io"
      :aria-label="t('component.registryHost')"
    />
    <NInput
      v-model:value="upstream.allow"
      type="textarea"
      :autosize="{minRows: 1, maxRows: 4}"
      placeholder="devcontainers/*"
      :aria-label="t('component.allowedRepositories')"
    />
    <NButton
      quaternary
      type="error"
      :aria-label="t('component.removeMirror')"
      :title="t('component.removeMirror')"
      @click="upstreams.splice(index, 1)"
    >
      <template #icon><X :size="16" /></template>
    </NButton>
  </div>
  <NButton
    class="add-row"
    @click="upstreams.push({host: '', allow: ''})"
  >
    <template #icon><Plus :size="16" /></template>
    {{ t('component.addMirror') }}
  </NButton>
  <h2>{{ t('proxy.section') }}</h2>
  <ProxySettings
    v-model="cache.proxy"
    :help="t('proxy.cacheHelp')"
  />
</template>
