import {computed, onBeforeUnmount, ref} from 'vue';
import {onBeforeRouteLeave, onBeforeRouteUpdate} from 'vue-router';

export function useDirtyForm(value: () => unknown) {
  const initial = ref('');
  const current = computed(() => JSON.stringify(value()));
  const dirty = computed(() => initial.value !== '' && current.value !== initial.value);
  const beforeUnload = (event: BeforeUnloadEvent) => {
    if (dirty.value) event.preventDefault();
  };
  window.addEventListener('beforeunload', beforeUnload);
  onBeforeUnmount(() => window.removeEventListener('beforeunload', beforeUnload));
  const confirmLeave = () => !dirty.value || window.confirm('Discard unsaved changes?');
  onBeforeRouteLeave(confirmLeave);
  onBeforeRouteUpdate(confirmLeave);
  return {
    dirty,
    reset: () => {
      initial.value = '';
    },
    saved: () => {
      initial.value = current.value;
    },
  };
}
