<script setup lang="ts">
import {computed, ref, watch} from 'vue';
import {NInputGroup, NInputGroupLabel, NInputNumber, NSelect} from 'naive-ui';
import {useI18n} from 'vue-i18n';
import {
  convertMeasurement,
  formatMeasurement,
  measurementUnits,
  parseMeasurement,
  type MeasurementKind,
} from '../utils/measurement';

const props = withDefaults(
  defineProps<{
    modelValue?: string;
    kind: MeasurementKind;
    ariaLabel?: string;
    allowEmpty?: boolean;
  }>(),
  {modelValue: '', ariaLabel: '', allowEmpty: false},
);
const emit = defineEmits<{(event: 'update:modelValue', value: string): void}>();
const {t} = useI18n();
const amount = ref<number | null>(null);
const unit = ref('');
const invalid = ref(false);

watch(
  () => [props.modelValue, props.kind] as const,
  () => {
    const parsed = parseMeasurement(props.modelValue, props.kind);
    amount.value = parsed.amount;
    unit.value = parsed.unit;
    invalid.value = !parsed.valid;
  },
  {immediate: true},
);

const unitLabels = computed<Record<string, string>>(() => ({
  count: t('units.items'),
  cpu: 'CPU',
  m: props.kind === 'duration' ? t('units.minutes') : 'mCPU',
  Mi: 'MiB',
  Gi: 'GiB',
  Ti: 'TiB',
  MiB: 'MiB',
  GiB: 'GiB',
  TiB: 'TiB',
  s: t('units.seconds'),
  h: t('units.hours'),
  d: t('units.days'),
}));
const unitOptions = computed(() =>
  measurementUnits(props.kind).map((value) => ({label: unitLabels.value[value], value})),
);
const precision = computed(() => {
  if (props.kind === 'count' || (props.kind === 'cpu' && unit.value === 'm')) return 0;
  return 3;
});
const minimum = computed(() => (precision.value === 0 ? 1 : 0.001));

function updateAmount(value: number | null) {
  amount.value = value;
  invalid.value = !props.allowEmpty && value === null;
  emit('update:modelValue', formatMeasurement(value, unit.value, props.kind));
}

function updateUnit(value: string) {
  if (amount.value !== null) amount.value = convertMeasurement(amount.value, unit.value, value, props.kind);
  unit.value = value;
  emit('update:modelValue', formatMeasurement(amount.value, unit.value, props.kind));
}
</script>

<template>
  <NInputGroup class="measurement-input">
    <NInputNumber
      :value="amount"
      :aria-label="ariaLabel"
      :min="minimum"
      :precision="precision"
      :status="invalid ? 'error' : undefined"
      @update:value="updateAmount"
    />
    <NInputGroupLabel v-if="kind === 'count'">{{ unitLabels.count }}</NInputGroupLabel>
    <NSelect
      v-else
      class="measurement-unit"
      :value="unit"
      :options="unitOptions"
      @update:value="updateUnit"
    />
  </NInputGroup>
</template>
