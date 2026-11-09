<script setup lang="ts">
import {computed} from 'vue';
import {NAlert, NButton, NCheckbox, NFormItem, NInput, NInputNumber, NSelect} from 'naive-ui';
import {Plus, Trash2} from '@lucide/vue';
import {useI18n} from 'vue-i18n';
import type {ClusterOptions, NativeRuntime} from '../api';
import {linuxCapabilities, normalizeCapabilities} from '../utils/capability';
import JsonEditor from './JsonEditor.vue';

const props = defineProps<{
  native: NativeRuntime;
  cluster: ClusterOptions;
}>();
const hostAccess = defineModel<string[]>('hostAccess', {required: true});
const {t} = useI18n();

const capabilityOptions = linuxCapabilities.map((value) => ({label: value, value}));
const requiredCapabilities = computed(() => new Set(props.cluster.requiredRuntimeCapabilities));
const runtimeCapabilityOptions = computed(() => {
  const values = new Set([...linuxCapabilities, ...props.cluster.requiredRuntimeCapabilities]);
  return [...values].sort().map((value) => ({
    label: requiredCapabilities.value.has(value) ? t('environment.requiredCapability', {name: value}) : value,
    value,
    disabled: requiredCapabilities.value.has(value),
  }));
});
const runtimeCapabilities = computed<string[]>({
  get: () => [...new Set([...props.cluster.requiredRuntimeCapabilities, ...props.native.runtimeCapAdd])],
  set(values) {
    props.native.runtimeCapAdd = normalizeCapabilities(values, requiredCapabilities.value);
  },
});
const deviceResourceOptions = computed(() =>
  props.cluster.deviceResources.map(({name, nodes, maxPerNode}) => ({
    label: `${name} (${t('environment.deviceCapacity', {maxPerNode, nodes})})`,
    value: name,
  })),
);

function addDevicePlugin() {
  props.native.devicePlugins.push({
    resourceName: props.cluster.deviceResources[0]?.name ?? '',
    count: 1,
    hostAccess: {},
  });
  hostAccess.value.push('{}');
}

function removeDevicePlugin(index: number) {
  props.native.devicePlugins.splice(index, 1);
  hostAccess.value.splice(index, 1);
}

function setCDI(index: number, enabled: boolean) {
  const device = props.native.devicePlugins[index];
  if (device) device.cdi = enabled ? {kind: '', deviceIDEnvironment: ''} : undefined;
}
</script>

<template>
  <h2>{{ t('environment.nativeSection') }}</h2>
  <p class="section-help">{{ t('environment.nativeHelp') }}</p>
  <div class="form-grid">
    <NFormItem
      :label="t('environment.runtimeCapabilities')"
      :feedback="t('environment.runtimeCapabilitiesHelp')"
    >
      <NSelect
        v-model:value="runtimeCapabilities"
        multiple
        filterable
        clearable
        tag
        max-tag-count="responsive"
        :options="runtimeCapabilityOptions"
      />
    </NFormItem>
    <NFormItem
      :label="t('environment.devContainerCapabilities')"
      :feedback="t('environment.devContainerCapabilitiesHelp')"
    >
      <NSelect
        v-model:value="native.devContainerCapAdd"
        multiple
        filterable
        clearable
        tag
        max-tag-count="responsive"
        :options="capabilityOptions"
      />
    </NFormItem>
  </div>
  <div class="subsection-heading">
    <div>
      <h3>{{ t('environment.devicePlugins') }}</h3>
      <p class="section-help">{{ t('environment.devicePluginsHelp') }}</p>
    </div>
    <NButton
      size="small"
      :disabled="!cluster.deviceResources.length"
      @click="addDevicePlugin"
    >
      <template #icon><Plus :size="16" /></template>
      {{ t('environment.addDevice') }}
    </NButton>
  </div>
  <NAlert
    v-if="!cluster.deviceResources.length"
    type="warning"
    class="notice"
  >
    {{ t('environment.noDeviceResources') }}
  </NAlert>
  <div
    v-for="(device, index) in native.devicePlugins"
    :key="`${device.resourceName}-${index}`"
    class="device-entry"
  >
    <div class="subsection-heading">
      <strong>{{ device.resourceName || t('environment.deviceResource') }}</strong>
      <NButton
        quaternary
        type="error"
        :aria-label="t('environment.removeDevice')"
        :title="t('environment.removeDevice')"
        @click="removeDevicePlugin(index)"
      >
        <template #icon><Trash2 :size="16" /></template>
      </NButton>
    </div>
    <div class="form-grid">
      <NFormItem
        :label="t('environment.deviceResource')"
        :feedback="t('environment.deviceResourceHelp')"
      >
        <NSelect
          v-model:value="device.resourceName"
          filterable
          :options="deviceResourceOptions"
        />
      </NFormItem>
      <NFormItem :label="t('environment.deviceCount')">
        <NInputNumber
          v-model:value="device.count"
          :min="1"
          :precision="0"
        />
      </NFormItem>
      <NFormItem>
        <NCheckbox v-model:checked="device.providesGPU">{{ t('environment.providesGPU') }}</NCheckbox>
      </NFormItem>
      <NFormItem>
        <NCheckbox
          :checked="Boolean(device.cdi)"
          @update:checked="setCDI(index, $event)"
        >
          {{ t('environment.cdiEnabled') }}
        </NCheckbox>
      </NFormItem>
      <template v-if="device.cdi">
        <NFormItem
          :label="t('environment.cdiKind')"
          :feedback="t('environment.cdiKindHelp')"
        >
          <NInput
            v-model:value="device.cdi.kind"
            placeholder="nvidia.com/gpu"
          />
        </NFormItem>
        <NFormItem
          :label="t('environment.deviceIDEnvironment')"
          :feedback="t('environment.deviceIDEnvironmentHelp')"
        >
          <NInput
            v-model:value="device.cdi.deviceIDEnvironment"
            placeholder="NVIDIA_VISIBLE_DEVICES"
          />
        </NFormItem>
      </template>
      <details class="field-wide advanced-fields">
        <summary>{{ t('environment.hostAccess') }}</summary>
        <NFormItem :feedback="t('environment.hostAccessHelp')">
          <JsonEditor
            v-model="hostAccess[index]"
            :label="t('environment.hostAccess')"
          />
        </NFormItem>
      </details>
    </div>
  </div>
</template>
