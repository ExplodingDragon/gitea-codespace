<script setup lang="ts">
import {NCheckbox, NFormItem, NInput} from 'naive-ui';
import {useI18n} from 'vue-i18n';
import type {Runtime} from '../api';
import JsonEditor from './JsonEditor.vue';

defineProps<{configuration: Runtime['devContainer']}>();
const settings = defineModel<string>('settings', {required: true});
const extensions = defineModel<string>('extensions', {required: true});
const customArchive = defineModel<boolean>('customArchive', {required: true});
const features = defineModel<string>('features', {required: true});
const {t} = useI18n();
</script>

<template>
  <h2>{{ t('environment.webIDESection') }}</h2>
  <p class="section-help">{{ t('environment.webIDEHelp') }}</p>
  <div class="form-grid">
    <NFormItem class="field-wide">
      <NCheckbox v-model:checked="configuration.webIDE.enabled">{{ t('environment.webIDEEnabled') }}</NCheckbox>
    </NFormItem>
    <template v-if="configuration.webIDE.enabled">
      <NFormItem class="field-wide">
        <NCheckbox v-model:checked="customArchive">{{ t('environment.webIDECustomArchive') }}</NCheckbox>
      </NFormItem>
      <NFormItem
        v-if="customArchive"
        class="field-wide"
        :label="t('environment.webIDEArchiveURL')"
      >
        <NInput
          v-model:value="configuration.webIDE.archiveURL"
          :placeholder="t('environment.webIDEArchiveURLPlaceholder')"
        />
        <template #feedback>{{ t('environment.webIDEArchiveURLHelp') }}</template>
      </NFormItem>
      <NFormItem
        class="field-wide"
        :label="t('environment.webIDESettings')"
      >
        <JsonEditor
          v-model="settings"
          :label="t('environment.jsonWebIDESettingsLabel')"
        />
      </NFormItem>
      <NFormItem
        class="field-wide"
        :label="t('environment.webIDEExtensions')"
      >
        <NInput
          v-model:value="extensions"
          type="textarea"
          :autosize="{minRows: 3, maxRows: 10}"
          :placeholder="t('environment.webIDEExtensionsPlaceholder')"
        />
        <template #feedback>{{ t('environment.webIDEExtensionsHelp') }}</template>
      </NFormItem>
    </template>
  </div>
  <h2>{{ t('environment.giteaCLISection') }}</h2>
  <p class="section-help">{{ t('environment.giteaCLIHelp') }}</p>
  <div class="form-grid">
    <NFormItem class="field-wide">
      <NCheckbox v-model:checked="configuration.giteaCLI">{{ t('environment.giteaCLIEnabled') }}</NCheckbox>
    </NFormItem>
  </div>
  <h2>{{ t('environment.featuresSection') }}</h2>
  <div class="form-grid">
    <NFormItem
      class="field-wide"
      :label="t('environment.features')"
    >
      <JsonEditor
        v-model="features"
        :label="t('environment.jsonFeaturesLabel')"
      />
    </NFormItem>
  </div>
  <h2>{{ t('environment.lifecycleSection') }}</h2>
  <p class="section-help">{{ t('environment.lifecycleHelp') }}</p>
  <div class="form-grid">
    <NFormItem
      class="field-wide"
      :label="t('environment.lifecycleScript')"
    >
      <NInput
        v-model:value="configuration.lifecycleScript"
        class="script-input"
        type="textarea"
        :autosize="{minRows: 10, maxRows: 24}"
        :maxlength="65536"
        show-count
        :placeholder="t('environment.lifecyclePlaceholder')"
        :aria-label="t('environment.lifecycleScript')"
      />
    </NFormItem>
  </div>
</template>
