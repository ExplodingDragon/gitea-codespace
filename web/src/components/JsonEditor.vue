<script setup lang="ts">
import {onBeforeUnmount, onMounted, ref, watch} from 'vue';
import {basicSetup} from 'codemirror';
import {json, jsonParseLinter} from '@codemirror/lang-json';
import {linter} from '@codemirror/lint';
import {EditorState} from '@codemirror/state';
import {EditorView} from '@codemirror/view';

const props = defineProps<{modelValue: string; label: string}>();
const emit = defineEmits<{(event: 'update:modelValue', value: string): void}>();
const root = ref<HTMLElement>();
let view: EditorView | undefined;

onMounted(() => {
  view = new EditorView({
    parent: root.value,
    state: EditorState.create({
      doc: props.modelValue,
      extensions: [
        basicSetup,
        json(),
        linter(jsonParseLinter()),
        EditorView.lineWrapping,
        EditorView.contentAttributes.of({'aria-label': props.label}),
        EditorView.updateListener.of((update) => {
          if (update.docChanged) emit('update:modelValue', update.state.doc.toString());
        }),
      ],
    }),
  });
});

watch(
  () => props.modelValue,
  (value) => {
    if (!view || value === view.state.doc.toString()) return;
    view.dispatch({changes: {from: 0, to: view.state.doc.length, insert: value}});
  },
);
watch(
  () => props.label,
  (label) => view?.contentDOM.setAttribute('aria-label', label),
);

onBeforeUnmount(() => view?.destroy());
</script>

<template>
  <div
    ref="root"
    class="json-editor"
  />
</template>

<style>
.json-editor {
  min-height: 140px;
  width: 100%;
  border: 1px solid var(--n-border-color, #d0d7de);
  border-radius: 4px;
  overflow: hidden;
}
.json-editor:focus-within {
  border-color: #238636;
  box-shadow: 0 0 0 2px rgb(35 134 54 / 18%);
}
.json-editor .cm-editor {
  min-height: 140px;
  font-size: 13px;
}
.json-editor .cm-scroller {
  overflow: auto;
}
</style>
