<script setup lang="ts">
import { ref, onMounted, onUnmounted } from 'vue'
import * as api from '../api'

const jobs = ref<api.Job[]>([])
const loading = ref(false)
const error = ref('')

const TAG: Record<api.Job['status'], string> = {
  queued: 'tag-yellow',
  running: 'tag-blue',
  succeeded: 'tag-green',
  failed: 'tag-red',
}

async function refresh() {
  loading.value = true
  try {
    jobs.value = await api.listJobs()
    error.value = ''
  } catch (e) {
    if (!(e instanceof api.ApiError && e.status === 401)) error.value = String(e)
  } finally {
    loading.value = false
  }
}

async function openPDF(id: string) {
  const url = await api.jobPDF(id)
  window.open(url, '_blank')
}

function fmtTime(iso: string): string {
  return new Date(iso).toLocaleString()
}

function fmtBytes(n?: number): string {
  if (!n) return ''
  return n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : `${Math.round(n / 1024)} KB`
}

let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  refresh()
  timer = setInterval(refresh, 5000)
})
onUnmounted(() => clearInterval(timer))
</script>

<template>
  <div class="page">
    <div class="page-head">
      <h2>Jobs</h2>
      <span v-if="error" class="tag tag-red">{{ error }}</span>
      <button class="btn btn-secondary" :disabled="loading" @click="refresh">Refresh</button>
    </div>

    <div class="jobs-card">
      <table class="table">
        <thead>
          <tr>
            <th>Job</th>
            <th>Template</th>
            <th>Version</th>
            <th>Status</th>
            <th>Size</th>
            <th>Created</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="j in jobs" :key="j.id">
            <td class="mono-cell">{{ j.id.slice(0, 8) }}</td>
            <td>
              {{ j.template }}
              <span v-if="j.sync" class="tag tag-mono">sync</span>
              <span v-if="j.archival_fallback" class="tag tag-yellow">fallback</span>
            </td>
            <td class="mono-cell">{{ j.template_version?.slice(0, 6) }}</td>
            <td>
              <span class="tag" :class="TAG[j.status]">{{ j.status }}</span>
              <span v-if="j.status === 'failed'" class="err-cell" :title="j.error">{{ j.error }}</span>
            </td>
            <td>{{ fmtBytes(j.pdf_bytes) }}</td>
            <td class="mono-cell">{{ fmtTime(j.created_at) }}</td>
            <td>
              <button
                v-if="j.status === 'succeeded'"
                class="btn btn-ghost"
                @click="openPDF(j.id)"
              >
                PDF
              </button>
            </td>
          </tr>
          <tr v-if="jobs.length === 0 && !loading">
            <td colspan="7" class="text-muted">No jobs yet — render something.</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
