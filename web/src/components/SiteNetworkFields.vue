<script setup lang="ts">
import {
  NButton,
  NCheckbox,
  NCheckboxGroup,
  NFormItem,
  NInput,
  NInputNumber,
  NRadioButton,
  NRadioGroup,
  NSelect,
} from 'naive-ui';
import {Plus, Trash2} from '@lucide/vue';
import {useI18n} from 'vue-i18n';
import type {SiteNetwork} from '../api';

const network = defineModel<SiteNetwork>({required: true});
const {t} = useI18n();
</script>

<template>
  <h2>{{ t('site.network') }}</h2>
  <NFormItem
    :label="t('site.ingress')"
    :feedback="t('site.sameSiteHelp')"
  >
    <NCheckbox v-model:checked="network.allowSameSite">{{ t('site.allowSameSite') }}</NCheckbox>
  </NFormItem>
  <NFormItem
    :label="t('site.egress')"
    :feedback="t('site.internetHelp')"
  >
    <NCheckbox v-model:checked="network.egress.allowInternet">{{ t('site.allowInternet') }}</NCheckbox>
  </NFormItem>
  <div
    v-for="(rule, index) in network.egress.allow"
    :key="index"
    class="network-rule"
  >
    <div class="form-grid">
      <NFormItem :label="t('site.cidr')">
        <NInput
          v-model:value="rule.cidr"
          placeholder="10.20.0.0/24"
          required
        />
      </NFormItem>
      <NFormItem :label="t('site.protocols')">
        <NCheckboxGroup v-model:value="rule.protocols">
          <div class="checkboxes">
            <NCheckbox value="TCP">TCP</NCheckbox>
            <NCheckbox value="UDP">UDP</NCheckbox>
          </div>
        </NCheckboxGroup>
      </NFormItem>
      <NFormItem
        :label="t('site.ports')"
        class="field-wide"
      >
        <NRadioGroup
          :value="rule.allPorts ? 'all' : 'selected'"
          @update:value="
            (value: string) => {
              rule.allPorts = value === 'all';
              rule.ports = rule.allPorts ? [] : [{port: 443}];
            }
          "
        >
          <NRadioButton value="selected">{{ t('site.selectedPorts') }}</NRadioButton>
          <NRadioButton value="all">{{ t('site.allPorts') }}</NRadioButton>
        </NRadioGroup>
      </NFormItem>
    </div>
    <template v-if="!rule.allPorts">
      <div
        v-for="(port, portIndex) in rule.ports"
        :key="portIndex"
        class="network-port-row"
      >
        <NFormItem :label="t('site.startPort')">
          <NInputNumber
            v-model:value="port.port"
            :min="1"
            :max="65535"
            :precision="0"
          />
        </NFormItem>
        <NFormItem :label="t('site.endPort')">
          <NInputNumber
            :value="port.endPort ?? null"
            :min="port.port || 1"
            :max="65535"
            :precision="0"
            clearable
            placeholder=""
            @update:value="
              (value: number | null) => {
                if (value === null) delete port.endPort;
                else port.endPort = value;
              }
            "
          />
        </NFormItem>
        <NButton
          quaternary
          :aria-label="t('site.removePort')"
          :title="t('site.removePort')"
          @click="rule.ports!.splice(portIndex, 1)"
        >
          <template #icon><Trash2 :size="16" /></template>
        </NButton>
      </div>
      <NButton
        quaternary
        :aria-label="t('site.addPort')"
        :title="t('site.addPort')"
        @click="(rule.ports ??= []).push({port: 443})"
      >
        <template #icon><Plus :size="16" /></template>
      </NButton>
    </template>
    <NButton
      quaternary
      type="error"
      :aria-label="t('site.removeDestination')"
      :title="t('site.removeDestination')"
      @click="network.egress.allow!.splice(index, 1)"
    >
      <template #icon><Trash2 :size="16" /></template>
    </NButton>
  </div>
  <NButton
    class="add-row"
    @click="(network.egress.allow ??= []).push({cidr: '', protocols: ['TCP'], allPorts: false, ports: [{port: 443}]})"
  >
    <template #icon><Plus :size="16" /></template>
    {{ t('site.addDestination') }}
  </NButton>
  <NFormItem
    :label="t('site.internalCIDRs')"
    :feedback="t('site.internalCIDRsHelp')"
  >
    <NSelect
      v-model:value="network.egress.internalCIDRs"
      multiple
      filterable
      tag
      :options="(network.egress.internalCIDRs ?? []).map((cidr) => ({label: cidr, value: cidr}))"
      placeholder="10.20.0.0/16"
    />
  </NFormItem>
</template>

<style scoped>
.network-rule {
  padding: 16px 0;
  border-bottom: 1px solid #d0d7de;
}
.network-port-row {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr) 34px;
  align-items: center;
  gap: 12px;
}
</style>
