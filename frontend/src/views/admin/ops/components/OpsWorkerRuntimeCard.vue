<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { OpsWorkerRuntimeStatus } from '@/api/admin/ops'

const props = defineProps<{
  status: OpsWorkerRuntimeStatus | null
  loading?: boolean
}>()

const { t } = useI18n()
const workers = computed(() => props.status?.workers ?? [])
const runningCount = computed(() => workers.value.filter((w) => w.Lifecycle?.State === 'running').length)
</script>

<template>
  <div class="rounded-2xl border border-gray-200/70 bg-[var(--glass-bg-content)] p-4 shadow-sm dark:border-white/10">
    <div class="mb-3 flex items-center justify-between">
      <h3 class="text-sm font-bold text-gray-900 dark:text-white">{{ t('admin.ops.workerRuntime.title') }}</h3>
      <span class="text-[10px] text-gray-500">{{ t('admin.ops.workerRuntime.scope') }}</span>
    </div>
    <div v-if="loading" class="h-8 animate-pulse rounded bg-gray-200 dark:bg-gray-700" />
    <div v-else-if="!status" class="text-xs text-gray-500">{{ t('admin.ops.workerRuntime.unavailable') }}</div>
    <div v-else class="space-y-2">
      <div class="text-xs text-gray-600 dark:text-gray-300">
        {{ t('admin.ops.workerRuntime.workers') }}: {{ runningCount }}/{{ workers.length }}
      </div>
      <div v-for="worker in workers" :key="worker.Descriptor.Name" class="flex items-center justify-between text-xs">
        <span class="font-mono text-gray-800 dark:text-gray-100">{{ worker.Descriptor.Name }}</span>
        <span>{{ worker.Lifecycle.State === 'running' ? t('admin.ops.workerRuntime.running') : t('admin.ops.workerRuntime.stopped') }}</span>
      </div>
    </div>
  </div>
</template>
