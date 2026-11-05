<script setup lang="ts">
import {computed} from 'vue';
import {NCheckbox, NFormItem, NInput} from 'naive-ui';
import {useI18n} from 'vue-i18n';
import type {OutboundProxy} from '../api';

const props = defineProps<{
  modelValue?: OutboundProxy;
}>();
const emit = defineEmits<{
  'update:modelValue': [value: OutboundProxy | undefined];
}>();
const {t} = useI18n();

const proxyURLFields = [
  {
    key: 'httpProxy',
    label: 'proxy.http',
    placeholder: 'http://username:password@proxy.example:3128',
    help: undefined,
  },
  {
    key: 'httpsProxy',
    label: 'proxy.https',
    placeholder: 'http://username:password@proxy.example:3128',
    help: undefined,
  },
  {
    key: 'socksProxy',
    label: 'proxy.socks',
    placeholder: 'socks5h://username:password@proxy.example:1080',
    help: 'proxy.socksHelp',
  },
] as const;

const enabled = computed({
  get: () => Boolean(props.modelValue),
  set: (value) => emit('update:modelValue', value ? {noProxy: []} : undefined),
});
const noProxy = computed({
  get: () => props.modelValue?.noProxy?.join('\n') ?? '',
  set: (value) =>
    update(
      'noProxy',
      value
        .split(/[\n,]/)
        .map((item) => item.trim())
        .filter(Boolean),
    ),
});

function update<K extends keyof OutboundProxy>(key: K, value: OutboundProxy[K]) {
  emit('update:modelValue', {...props.modelValue, [key]: value});
}
</script>

<template>
  <NFormItem>
    <NCheckbox v-model:checked="enabled">{{ t('proxy.enabled') }}</NCheckbox>
  </NFormItem>
  <template v-if="modelValue">
    <p class="section-help">{{ t('proxy.help') }}</p>
    <div class="form-grid">
      <NFormItem
        v-for="field in proxyURLFields"
        :key="field.key"
        class="field-wide"
        :label="t(field.label)"
        :feedback="field.help ? `${t('proxy.urlHelp')} ${t(field.help)}` : t('proxy.urlHelp')"
      >
        <NInput
          :value="modelValue[field.key]"
          :placeholder="field.placeholder"
          @update:value="update(field.key, $event)"
        />
      </NFormItem>
      <NFormItem
        class="field-wide"
        :label="t('proxy.noProxy')"
        :feedback="t('proxy.noProxyHelp')"
      >
        <NInput
          v-model:value="noProxy"
          type="textarea"
          :autosize="{minRows: 3, maxRows: 8}"
          placeholder="registry.internal.example&#10;10.0.0.0/8"
        />
      </NFormItem>
      <NFormItem
        class="field-wide"
        :label="t('proxy.caBundle')"
        :feedback="t('proxy.caBundleHelp')"
      >
        <NInput
          :value="modelValue.caBundle"
          type="textarea"
          :autosize="{minRows: 4, maxRows: 12}"
          placeholder="-----BEGIN CERTIFICATE-----"
          @update:value="update('caBundle', $event)"
        />
      </NFormItem>
    </div>
  </template>
</template>
