<script setup lang="ts">
import { ref } from 'vue'
import { useDesigner } from '../state/designer'
import { navigate } from '../router'

const designer = useDesigner()
const newName = ref('')
const emit = defineEmits<{ collapse: [] }>()

function create() {
  const name = newName.value.trim()
  if (!name) return
  designer.create(name)
  newName.value = ''
  navigate({ name: 'templates', template: name })
}
</script>

<template>
  <aside class="rail">
    <div class="rail-head">
      <span class="eyebrow">Templates</span>
      <span class="tag tag-neutral">{{ designer.templates.length }}</span>
      <button class="btn btn-ghost rail-toggle" title="Hide template list" @click="emit('collapse')">«</button>
    </div>
    <div class="rail-list">
      <button
        v-for="t in designer.templates"
        :key="t.name"
        class="rail-item"
        :class="{ on: t.name === designer.doc?.name }"
        @click="navigate({ name: 'templates', template: t.name })"
      >
        <span class="name">{{ t.name }}</span>
        <span class="ver">{{ t.version.slice(0, 6) }}</span>
      </button>
    </div>
    <div class="rail-new">
      <input v-model="newName" class="input" placeholder="new-template-name" @keydown.enter="create" />
      <button class="btn btn-secondary btn-block" :disabled="!newName.trim()" @click="create">
        New template
      </button>
    </div>
  </aside>
</template>
