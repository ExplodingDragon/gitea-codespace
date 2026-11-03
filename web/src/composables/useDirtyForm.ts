import {computed, onBeforeUnmount, ref} from 'vue';
import {onBeforeRouteLeave, onBeforeRouteUpdate} from 'vue-router';
import {useI18n} from 'vue-i18n';

export function useDirtyForm(value: () => unknown) {
  const {t} = useI18n();
  const initial = ref('');
  const current = computed(() => JSON.stringify(value()));
  const dirty = computed(() => initial.value !== '' && current.value !== initial.value);
  const beforeUnload = (event: BeforeUnloadEvent) => {
    if (dirty.value) event.preventDefault();
  };
  window.addEventListener('beforeunload', beforeUnload);
  onBeforeUnmount(() => window.removeEventListener('beforeunload', beforeUnload));
  const confirmDiscard = () => !dirty.value || window.confirm(t('common.discard'));
  onBeforeRouteLeave(confirmDiscard);
  onBeforeRouteUpdate(confirmDiscard);
  return {
    confirmDiscard,
    endTracking: () => {
      initial.value = '';
    },
    beginTracking: () => {
      initial.value = current.value;
    },
  };
}
