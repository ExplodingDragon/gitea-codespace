<script setup lang="ts">
import {computed} from 'vue';
import {NCheckbox, NFormItem, NInput} from 'naive-ui';
import {useI18n} from 'vue-i18n';
import type {OutboundProxy} from '../api';

const props = defineProps<{
  modelValue?: OutboundProxy;
  username: string;
  password: string;
  clearCredential: boolean;
}>();
const emit = defineEmits<{
  'update:modelValue': [value: OutboundProxy | undefined];
  'update:username': [value: string];
  'update:password': [value: string];
  'update:clearCredential': [value: boolean];
}>();
const {t} = useI18n();

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
    <div class="fields">
      <NFormItem
        :label="t('proxy.http')"
        :feedback="t('proxy.httpHelp')"
      >
        <NInput
          :value="modelValue.httpProxy"
          placeholder="http://proxy.example:3128"
          @update:value="update('httpProxy', $event)"
        />
      </NFormItem>
      <NFormItem :label="t('proxy.https')">
        <NInput
          :value="modelValue.httpsProxy"
          placeholder="http://proxy.example:3128"
          @update:value="update('httpsProxy', $event)"
        />
      </NFormItem>
      <NFormItem
        :label="t('proxy.socks')"
        :feedback="t('proxy.socksHelp')"
      >
        <NInput
          :value="modelValue.socksProxy"
          placeholder="socks5h://proxy.example:1080"
          @update:value="update('socksProxy', $event)"
        />
      </NFormItem>
      <NFormItem
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
      <NFormItem :label="t('proxy.username')">
        <NInput
          :value="username"
          autocomplete="off"
          @update:value="emit('update:username', $event)"
        />
      </NFormItem>
      <NFormItem :label="t('proxy.password')">
        <NInput
          :value="password"
          type="password"
          autocomplete="new-password"
          @update:value="emit('update:password', $event)"
        />
      </NFormItem>
    </div>
    <NFormItem>
      <NCheckbox
        :checked="clearCredential"
        @update:checked="emit('update:clearCredential', $event)"
      >
        {{ t('proxy.clearCredential') }}
      </NCheckbox>
    </NFormItem>
    <NFormItem
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
  </template>
</template>
