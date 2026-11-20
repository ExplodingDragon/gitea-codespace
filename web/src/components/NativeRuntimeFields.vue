<script setup lang="ts">
import {computed} from 'vue';
import {
  NAlert,
  NButton,
  NCheckbox,
  NDynamicInput,
  NDynamicTags,
  NFormItem,
  NInput,
  NInputNumber,
  NSelect,
} from 'naive-ui';
import {ListPlus, Plus, Trash2} from '@lucide/vue';
import {useI18n} from 'vue-i18n';
import type {ClusterOptions, NativeRuntime} from '../api';
import {linuxCapabilities, normalizeCapabilities} from '../utils/capability';

const props = defineProps<{
  native: NativeRuntime;
  cluster: ClusterOptions;
}>();
const {t} = useI18n();
type Device = NativeRuntime['devicePlugins'][number];

const selectionOptions = computed(() => [
  {label: t('environment.cdiEnvironment'), value: 'environment'},
  {label: t('environment.cdiDeviceNodes'), value: 'deviceNodes'},
]);
const scopeOptions = computed(() => [
  {label: 'Runtime', value: 'runtime'},
  {label: t('environment.nodeSource'), value: 'node'},
]);

function fillSuggestedMapping(device: Device) {
  const access = (device.hostAccess ??= {});
  if (['nvidia.com/gpu', 'gpu.intel.com/i915', 'gpu.intel.com/xe'].includes(device.resourceName)) {
    if (!access.devices?.length) device.cdi ??= defaultCDI(device.resourceName);
    device.providesGPU ??= true;
  } else if (device.resourceName === 'amd.com/gpu') {
    if (!access.devices?.length && !device.cdi) {
      access.devices = [
        {source: '/dev/dri', target: '/dev/dri', permissions: 'rwm'},
        {source: '/dev/kfd', target: '/dev/kfd', permissions: 'rwm'},
      ];
    }
    device.providesGPU ??= true;
  }
}

function defaultCDI(resource: string): NonNullable<Device['cdi']> {
  if (['gpu.intel.com/i915', 'gpu.intel.com/xe'].includes(resource))
    return {kind: 'intel.cdi.k8s.io/gpu', selection: 'deviceNodes', deviceNodePaths: ['/dev/dri/card*']};
  return {
    kind: resource === 'nvidia.com/gpu' ? 'nvidia.com/gpu' : '',
    selection: 'environment',
    deviceIDEnvironment: resource === 'nvidia.com/gpu' ? 'NVIDIA_VISIBLE_DEVICES' : '',
  };
}

function setCDISelection(device: Device, selection: 'environment' | 'deviceNodes') {
  if (!device.cdi) return;
  device.cdi.selection = selection;
  if (selection === 'environment') {
    delete device.cdi.deviceNodePaths;
    device.cdi.deviceIDEnvironment ??= '';
  } else {
    delete device.cdi.deviceIDEnvironment;
    device.cdi.deviceNodePaths ??= device.resourceName.startsWith('gpu.intel.com/') ? ['/dev/dri/card*'] : [];
  }
}

const capabilityOptions = linuxCapabilities.map((value) => ({label: value, value}));
const devicePermissionOptions = ['r', 'w', 'm', 'rw', 'rm', 'wm', 'rwm'].map((value) => ({label: value, value}));
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
  const device: Device = {
    resourceName: props.cluster.deviceResources[0]?.name ?? '',
    count: 1,
    hostAccess: {},
  };
  fillSuggestedMapping(device);
  props.native.devicePlugins.push(device);
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
  <NFormItem :label="t('environment.additionalGroups')">
    <NDynamicInput
      v-model:value="native.additionalGroups"
      :on-create="() => 1"
    >
      <template #default="{index}">
        <NInputNumber
          v-model:value="native.additionalGroups![index]"
          :min="1"
          :max="4294967294"
          :precision="0"
        />
      </template>
    </NDynamicInput>
  </NFormItem>
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
        @click="native.devicePlugins.splice(index, 1)"
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
          @update:checked="device.cdi = $event ? defaultCDI(device.resourceName) : undefined"
        >
          {{ t('environment.cdiEnabled') }}
        </NCheckbox>
      </NFormItem>
      <template v-if="device.cdi">
        <NFormItem :label="t('environment.cdiSelection')">
          <NSelect
            :value="device.cdi.selection"
            :options="selectionOptions"
            @update:value="setCDISelection(device, $event)"
          />
        </NFormItem>
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
          v-if="device.cdi.selection === 'environment'"
          :label="t('environment.deviceIDEnvironment')"
          :feedback="t('environment.deviceIDEnvironmentHelp')"
        >
          <NInput
            v-model:value="device.cdi.deviceIDEnvironment"
            placeholder="NVIDIA_VISIBLE_DEVICES"
          />
        </NFormItem>
        <NFormItem
          v-else
          :label="t('environment.deviceNodePaths')"
          :feedback="t('environment.deviceNodePathsHelp')"
        >
          <NDynamicTags v-model:value="device.cdi.deviceNodePaths" />
        </NFormItem>
      </template>
    </div>
    <NButton
      size="small"
      class="add-row"
      @click="fillSuggestedMapping(device)"
    >
      <template #icon><ListPlus :size="16" /></template>
      {{ t('environment.fillDevicePreset') }}
    </NButton>
    <template v-if="device.hostAccess">
      <NFormItem :label="t('environment.deviceMappings')">
        <NDynamicInput
          v-model:value="device.hostAccess.devices"
          :on-create="() => ({source: '', target: '', permissions: 'rwm'})"
        >
          <template #default="{value}">
            <div class="mapping-fields">
              <NInput
                v-model:value="value.source"
                :placeholder="t('environment.runtimeDeviceSource')"
                :aria-label="t('environment.runtimeDeviceSource')"
              />
              <NInput
                v-model:value="value.target"
                :placeholder="t('environment.containerTarget')"
                :aria-label="t('environment.containerTarget')"
              />
              <NSelect
                v-model:value="value.permissions"
                :options="devicePermissionOptions"
                :aria-label="t('environment.devicePermissions')"
              />
            </div>
          </template>
        </NDynamicInput>
      </NFormItem>
      <NFormItem :label="t('environment.fileMappings')">
        <NDynamicInput
          v-model:value="device.hostAccess.mounts"
          :on-create="() => ({sourceScope: 'runtime', source: '', target: '', readOnly: true})"
        >
          <template #default="{value}">
            <div class="mapping-fields">
              <NSelect
                v-model:value="value.sourceScope"
                :options="scopeOptions"
                :aria-label="t('environment.mountSourceScope')"
              />
              <NInput
                v-model:value="value.source"
                :placeholder="t('environment.sourcePath')"
                :aria-label="t('environment.sourcePath')"
              />
              <NInput
                v-model:value="value.target"
                :placeholder="t('environment.containerTarget')"
                :aria-label="t('environment.containerTarget')"
              />
              <NCheckbox v-model:checked="value.readOnly">{{ t('environment.readOnlyMount') }}</NCheckbox>
            </div>
          </template>
        </NDynamicInput>
      </NFormItem>
      <NFormItem :label="t('environment.forwardEnvironment')">
        <NDynamicTags v-model:value="device.hostAccess.forwardEnvironment" />
      </NFormItem>
    </template>
  </div>
</template>

<style scoped>
.mapping-fields {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 8px;
  width: 100%;
  min-width: 0;
}
@media (width <= 600px) {
  .mapping-fields {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
