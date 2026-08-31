<script setup lang="ts">
import { useDesigner } from '../state/designer'
import CodeEditor from './CodeEditor.vue'
import PageSetupForm from './PageSetupForm.vue'

const designer = useDesigner()
defineProps<{ railCollapsed: boolean }>()
const emit = defineEmits<{ expand: []; delete: [] }>()
</script>

<template>
  <div class="pane-head">
    <button
      v-if="railCollapsed"
      class="btn btn-ghost rail-toggle"
      title="Show template list"
      @click="emit('expand')"
    >»</button>
    <div class="seg" role="tablist">
      <label class="seg-opt"><input v-model="designer.tab" type="radio" value="source" />Source</label>
      <label class="seg-opt"><input v-model="designer.tab" type="radio" value="defaults" />Defaults</label>
      <label class="seg-opt"><input v-model="designer.tab" type="radio" value="example" />Example</label>
      <label class="seg-opt"><input v-model="designer.tab" type="radio" value="page" />Page</label>
    </div>
    <span class="spacer"></span>
    <button
      v-if="designer.missingKeys.length"
      class="tag tag-yellow key-audit"
      :title="`Referenced in the source but absent from defaults (save would be rejected): ${designer.missingKeys.join(', ')}. Click to add placeholders.`"
      @click="designer.addMissingDefaults()"
    >
      {{ designer.missingKeys.length }} missing default{{ designer.missingKeys.length === 1 ? '' : 's' }} · fix
    </button>
    <span v-if="designer.notice" class="tag tag-green">{{ designer.notice }}</span>
    <button class="btn btn-ghost" @click="emit('delete')">Delete</button>
    <button class="btn btn-primary" :disabled="designer.busy || !designer.dirty" @click="designer.save()">
      <span v-if="designer.dirty" class="dirty-dot" aria-hidden="true"></span>
      Save
    </button>
  </div>
  <div v-if="designer.doc" class="editor">
    <CodeEditor
      v-if="designer.tab === 'source'"
      v-model="designer.doc.source"
      lang="typst"
      :docs="designer.editorDocs"
      :jump="designer.jump"
    />
    <CodeEditor v-else-if="designer.tab === 'defaults'" v-model="designer.doc.defaults" lang="json" />
    <CodeEditor v-else-if="designer.tab === 'example'" v-model="designer.doc.example" lang="json" />
    <PageSetupForm v-else />
  </div>
</template>
