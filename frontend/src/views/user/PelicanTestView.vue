<template>
  <AppLayout>
    <div class="space-y-6">
      <div>
        <h1 class="text-2xl font-semibold text-gray-900 dark:text-gray-100">{{ t('nav.pelicanTest') }}</h1>
        <p class="mt-2 max-w-3xl text-sm leading-relaxed text-gray-500 dark:text-gray-400">{{ t('pelicanTest.subtitle') }}</p>
      </div>

      <div v-if="loading" class="flex justify-center py-16"><LoadingSpinner /></div>
      <p v-else-if="error" class="text-sm text-red-600" role="alert">{{ error }}</p>
      <p v-else-if="!item" class="rounded-2xl border border-dashed border-gray-200 p-10 text-center text-sm text-gray-400 dark:border-dark-700">{{ t('pelicanTest.empty') }}</p>
      <article v-else class="mx-auto max-w-4xl overflow-hidden rounded-2xl border border-gray-200 bg-white shadow-sm dark:border-dark-700 dark:bg-dark-800">
        <div v-if="item.html" class="aspect-[4/3] bg-[#0b1220]">
          <iframe
            class="h-full w-full border-0"
            sandbox=""
            referrerpolicy="no-referrer"
            :title="t('nav.pelicanTest')"
            :srcdoc="item.html"
          />
        </div>
        <p v-else class="flex aspect-[4/3] items-center justify-center px-6 text-center text-sm text-gray-400">{{ t('pelicanTest.htmlUnavailable') }}</p>
        <dl class="grid grid-cols-2 gap-3 px-4 py-4 text-sm sm:grid-cols-4">
          <div>
            <dt class="text-xs text-gray-400">{{ t('pelicanTest.time') }}</dt>
            <dd class="mt-1 text-gray-800 dark:text-gray-100">{{ formatTime(item.finished_at || item.created_at) }}</dd>
          </div>
          <div>
            <dt class="text-xs text-gray-400">{{ t('pelicanTest.group') }}</dt>
            <dd class="mt-1 text-gray-800 dark:text-gray-100">{{ item.group_name || 'GPT-PRO' }}</dd>
          </div>
          <div>
            <dt class="text-xs text-gray-400">{{ t('pelicanTest.model') }}</dt>
            <dd class="mt-1 text-gray-800 dark:text-gray-100">{{ item.model || 'gpt-6-astra' }}</dd>
          </div>
          <div>
            <dt class="text-xs text-gray-400">{{ t('pelicanTest.reasoning') }}</dt>
            <dd class="mt-1 text-gray-800 dark:text-gray-100">{{ item.reasoning_effort || 'low' }}</dd>
          </div>
        </dl>
        <div v-if="item.prompt" class="border-t border-gray-100 px-4 py-4 dark:border-dark-700">
          <p class="text-xs text-gray-400">{{ t('pelicanTest.prompt') }}</p>
          <pre class="mt-2 whitespace-pre-wrap break-words text-sm leading-relaxed text-gray-700 dark:text-gray-200">{{ item.prompt }}</pre>
        </div>
      </article>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import { pelicanTestsAPI, type PelicanTestItem } from '@/api/pelicanTests'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()
const loading = ref(true)
const error = ref('')
const item = ref<PelicanTestItem | null>(null)

function formatTime(value?: string | null) {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isFinite(date.getTime()) ? date.toLocaleString(undefined, { hour12: false }) : '—'
}

onMounted(async () => {
  try {
    const page = await pelicanTestsAPI.list(1, 1)
    item.value = page.items?.[0] ?? null
  } catch (err) {
    error.value = extractApiErrorMessage(err, t('pelicanTest.loadFailed'))
  } finally {
    loading.value = false
  }
})
</script>
