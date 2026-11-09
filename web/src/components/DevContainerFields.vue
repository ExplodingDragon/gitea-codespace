<script setup lang="ts">
import {computed, ref, watch} from 'vue';
import Ajv2020 from 'ajv/dist/2020.js';
import {Search} from '@lucide/vue';
import {NAlert, NAutoComplete, NButton, NCheckbox, NFormItem, NIcon, NInput, NTag} from 'naive-ui';
import {useI18n} from 'vue-i18n';
import {errorMessage, request, type ClusterOptions, type ResolvedWebIDEImage, type Runtime} from '../api';
import JsonEditor from './JsonEditor.vue';

const props = defineProps<{
  configuration: Runtime['devContainer'];
  imageSuggestions: ClusterOptions['webIDEImageSuggestions'];
}>();
const emit = defineEmits<{(event: 'validation', message: string): void}>();
const options = defineModel<string>('options', {required: true});
const features = defineModel<string>('features', {required: true});
const {t} = useI18n();
const resolving = ref(false);
const resolveError = ref('');
const resolved = ref<ResolvedWebIDEImage>();
const resolvedInput = ref('');

const imageOptions = computed(() => props.imageSuggestions.map((value) => ({label: value, value})));
const optionsValidator = computed(() => {
  if (!resolved.value) return undefined;
  return new Ajv2020({allErrors: true, strict: false}).compile(resolved.value.optionsSchema);
});
const optionsValidation = computed(() => {
  const validate = optionsValidator.value;
  if (!validate) return '';
  try {
    const value: unknown = JSON.parse(options.value);
    if (!value || Array.isArray(value) || typeof value !== 'object') return t('environment.invalidWebIDEOptions');
    if (validate(value)) return '';
    return (
      validate.errors?.map(({instancePath, message}) => `${instancePath || '/'} ${message ?? ''}`.trim()).join('\n') ??
      ''
    );
  } catch {
    return t('environment.invalidWebIDEOptions');
  }
});

watch(optionsValidation, (message) => emit('validation', message), {immediate: true});

function updateImage(image: string) {
  props.configuration.webIDE.image = image;
  if (image !== resolvedInput.value) {
    delete props.configuration.webIDE.provider;
    resolved.value = undefined;
    resolveError.value = '';
  }
}

async function resolveImage() {
  const image = props.configuration.webIDE.image?.trim() ?? '';
  if (!image) {
    props.configuration.webIDE = {};
    options.value = '{}';
    resolved.value = undefined;
    resolvedInput.value = '';
    resolveError.value = '';
    return;
  }
  resolving.value = true;
  resolveError.value = '';
  try {
    const result = await request<ResolvedWebIDEImage>('web-ide-images/resolve', 'POST', {image});
    props.configuration.webIDE.image = result.image;
    props.configuration.webIDE.provider = result.provider;
    props.configuration.webIDE.options = result.optionDefaults;
    options.value = JSON.stringify(result.optionDefaults, null, 2);
    resolved.value = result;
    resolvedInput.value = result.image;
  } catch (error) {
    resolveError.value = errorMessage(error, t);
  } finally {
    resolving.value = false;
  }
}
</script>

<template>
  <h2>{{ t('environment.webIDESection') }}</h2>
  <p class="section-help">{{ t('environment.webIDEHelp') }}</p>
  <div class="form-grid">
    <NFormItem
      class="field-wide"
      :label="t('environment.webIDEImage')"
    >
      <div class="web-ide-image-field">
        <NAutoComplete
          :value="configuration.webIDE.image ?? ''"
          :options="imageOptions"
          :placeholder="t('environment.webIDEImagePlaceholder')"
          clearable
          @update:value="updateImage"
        />
        <NButton
          :loading="resolving"
          :disabled="!(configuration.webIDE.image ?? '').trim()"
          @click="resolveImage"
        >
          <template #icon><NIcon :component="Search" /></template>
          {{ t('environment.resolveWebIDEImage') }}
        </NButton>
      </div>
      <template #feedback>{{ t('environment.webIDEImageHelp') }}</template>
    </NFormItem>
    <NAlert
      v-if="resolveError"
      class="field-wide"
      type="error"
      :title="t('environment.webIDEImageInvalid')"
    >
      {{ resolveError }}
    </NAlert>
    <div
      v-if="resolved"
      class="field-wide web-ide-resolution"
    >
      <strong>{{ resolved.providerName }}</strong>
      <NTag size="small">{{ resolved.version }}</NTag>
      <NTag
        v-for="platform in resolved.platforms"
        :key="platform"
        size="small"
        :bordered="false"
      >
        {{ platform }}
      </NTag>
      <code>{{ resolved.image.split('@')[1] }}</code>
    </div>
    <NFormItem
      v-if="configuration.webIDE.image"
      class="field-wide"
      :label="t('environment.webIDEOptions')"
      :validation-status="optionsValidation ? 'error' : undefined"
      :feedback="optionsValidation || t('environment.webIDEOptionsHelp')"
    >
      <JsonEditor
        v-model="options"
        :label="t('environment.jsonWebIDEOptionsLabel')"
      />
    </NFormItem>
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
