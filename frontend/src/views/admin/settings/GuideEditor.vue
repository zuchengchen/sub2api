<template>
  <div class="space-y-6" data-test="guide-editor">
    <div class="card">
      <div class="flex flex-col gap-4 border-b border-gray-100 px-6 py-4 dark:border-dark-700 sm:flex-row sm:items-start sm:justify-between">
        <div>
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
            {{ t('admin.settings.guideEditor.title') }}
          </h2>
          <p class="mt-1 max-w-3xl text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.settings.guideEditor.description') }}
          </p>
        </div>
        <a
          href="/guide"
          target="_blank"
          rel="noopener noreferrer"
          class="btn btn-secondary btn-sm inline-flex flex-shrink-0 items-center gap-2"
        >
          <Icon name="externalLink" size="sm" />
          {{ t('admin.settings.guideEditor.openGuide') }}
        </a>
      </div>

      <div v-if="loading" class="flex min-h-80 items-center justify-center gap-2 p-6 text-sm text-gray-500 dark:text-gray-400">
        <span class="h-5 w-5 animate-spin rounded-full border-2 border-gray-300 border-t-primary-600"></span>
        {{ t('common.loading') }}
      </div>

      <div v-else class="space-y-5 p-6">
        <div class="flex flex-wrap items-center gap-x-5 gap-y-2 border-y border-gray-200 py-3 text-sm dark:border-dark-700">
          <span class="font-medium text-gray-800 dark:text-gray-200">
            {{ t('admin.settings.guideEditor.currentVersion', { version }) }}
          </span>
          <span :class="hasCustomContent ? 'text-emerald-700 dark:text-emerald-300' : 'text-gray-500 dark:text-gray-400'">
            {{ hasCustomContent ? t('admin.settings.guideEditor.databaseContent') : t('admin.settings.guideEditor.bundledContent') }}
          </span>
          <span v-if="updatedAt" class="text-gray-500 dark:text-gray-400">
            {{ t('admin.settings.guideEditor.updatedAt', { date: formatDate(updatedAt) }) }}
          </span>
        </div>

        <div class="grid min-w-0 gap-5 lg:grid-cols-2">
          <section class="min-w-0" aria-labelledby="guide-markdown-label">
            <div class="mb-2 flex items-center justify-between gap-3">
              <label id="guide-markdown-label" for="guide-markdown" class="text-sm font-semibold text-gray-800 dark:text-gray-200">
                {{ t('admin.settings.guideEditor.markdown') }}
              </label>
              <span class="text-xs" :class="contentTooLarge ? 'text-red-600 dark:text-red-400' : 'text-gray-500 dark:text-gray-400'">
                {{ t('admin.settings.guideEditor.size', { size: formattedSize }) }}
              </span>
            </div>
            <textarea
              id="guide-markdown"
              v-model="editorContent"
              class="input min-h-[36rem] resize-y whitespace-pre-wrap font-mono text-sm leading-6"
              :placeholder="t('admin.settings.guideEditor.placeholder')"
              spellcheck="false"
              data-test="guide-markdown"
            ></textarea>
          </section>

          <section class="min-w-0" aria-labelledby="guide-preview-label">
            <div class="mb-2 flex items-center justify-between gap-3">
              <h3 id="guide-preview-label" class="text-sm font-semibold text-gray-800 dark:text-gray-200">
                {{ t('admin.settings.guideEditor.preview') }}
              </h3>
              <span class="inline-flex items-center gap-1 text-xs text-gray-500 dark:text-gray-400">
                <Icon name="eye" size="xs" />
                {{ t('admin.settings.guideEditor.sanitizedPreview') }}
              </span>
            </div>
            <article
              class="guide-preview min-h-[36rem] max-h-[52rem] overflow-y-auto border border-gray-200 bg-white p-5 dark:border-dark-700 dark:bg-dark-900"
              data-test="guide-preview"
              v-html="previewHtml"
            ></article>
          </section>
        </div>

        <p v-if="contentTooLarge" class="text-sm text-red-600 dark:text-red-400">
          {{ t('admin.settings.guideEditor.tooLarge') }}
        </p>

        <div class="flex flex-col-reverse gap-3 border-t border-gray-200 pt-5 dark:border-dark-700 sm:flex-row sm:items-center sm:justify-between">
          <button
            type="button"
            class="btn btn-secondary"
            :disabled="saving || !hasCustomContent"
            @click="confirmReset = true"
          >
            <Icon name="refresh" size="sm" />
            {{ t('admin.settings.guideEditor.useBundled') }}
          </button>
          <div class="flex flex-col-reverse gap-3 sm:flex-row sm:items-center">
            <span v-if="dirty" class="text-xs text-amber-700 dark:text-amber-300">
              {{ t('admin.settings.guideEditor.unsaved') }}
            </span>
            <button
              type="button"
              class="btn btn-primary"
              :disabled="saving || !dirty || !editorContent.trim() || contentTooLarge"
              data-test="guide-save"
              @click="saveGuide"
            >
              <span v-if="saving" class="h-4 w-4 animate-spin rounded-full border-2 border-white/40 border-t-white"></span>
              <Icon v-else name="check" size="sm" />
              {{ saving ? t('admin.settings.guideEditor.saving') : t('admin.settings.guideEditor.publish') }}
            </button>
          </div>
        </div>
      </div>
    </div>

    <div v-if="!loading" class="card">
      <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
        <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
          {{ t('admin.settings.guideEditor.historyTitle') }}
        </h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.settings.guideEditor.historyDescription') }}
        </p>
      </div>
      <div v-if="!sortedRevisions.length" class="px-6 py-10 text-center text-sm text-gray-500 dark:text-gray-400">
        {{ t('admin.settings.guideEditor.noHistory') }}
      </div>
      <div v-else class="divide-y divide-gray-200 dark:divide-dark-700">
        <div
          v-for="revision in sortedRevisions"
          :key="revision.version"
          class="flex flex-col gap-3 px-6 py-4 sm:flex-row sm:items-center sm:justify-between"
        >
          <div class="min-w-0">
            <div class="flex flex-wrap items-center gap-2">
              <span class="font-medium text-gray-900 dark:text-white">
                {{ t('admin.settings.guideEditor.revision', { version: revision.version }) }}
              </span>
              <span v-if="revision.version === version" class="rounded bg-primary-50 px-2 py-0.5 text-xs font-medium text-primary-700 dark:bg-primary-500/10 dark:text-primary-300">
                {{ t('admin.settings.guideEditor.current') }}
              </span>
              <span class="text-xs text-gray-500 dark:text-gray-400">
                {{ revision.content.trim() ? t('admin.settings.guideEditor.databaseContent') : t('admin.settings.guideEditor.bundledContent') }}
              </span>
            </div>
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
              {{ formatDate(revision.updated_at) }}
            </p>
          </div>
          <button
            v-if="revision.version !== version"
            type="button"
            class="btn btn-secondary btn-sm flex-shrink-0"
            :disabled="saving"
            @click="pendingRevision = revision"
          >
            <Icon name="refresh" size="sm" />
            {{ t('admin.settings.guideEditor.restore') }}
          </button>
        </div>
      </div>
    </div>

    <ConfirmDialog
      :show="confirmReset"
      :title="t('admin.settings.guideEditor.resetTitle')"
      :message="t('admin.settings.guideEditor.resetMessage')"
      :confirm-text="t('admin.settings.guideEditor.useBundled')"
      @confirm="resetGuide"
      @cancel="confirmReset = false"
    />
    <ConfirmDialog
      :show="pendingRevision !== null"
      :title="t('admin.settings.guideEditor.restoreTitle')"
      :message="t('admin.settings.guideEditor.restoreMessage', { version: pendingRevision?.version ?? '' })"
      :confirm-text="t('admin.settings.guideEditor.restore')"
      @confirm="restoreRevision"
      @cancel="pendingRevision = null"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api'
import type { GuideRevision, GuideSettings } from '@/api/admin/settings'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore } from '@/stores'
import { extractApiErrorCode, extractApiErrorMessage } from '@/utils/apiError'
import { buildGuideDocument } from '@/utils/guideMarkdown'
import bundledGuide from '../../../../../docs/guide.zh.md?raw'

const maxContentBytes = 256 * 1024
const { t, locale } = useI18n()
const appStore = useAppStore()
const loading = ref(true)
const saving = ref(false)
const editorContent = ref('')
const savedEditorContent = ref('')
const version = ref(0)
const updatedAt = ref('')
const hasCustomContent = ref(false)
const revisions = ref<GuideRevision[]>([])
const confirmReset = ref(false)
const pendingRevision = ref<GuideRevision | null>(null)

const contentBytes = computed(() => new TextEncoder().encode(editorContent.value).byteLength)
const contentTooLarge = computed(() => contentBytes.value > maxContentBytes)
const formattedSize = computed(() => `${(contentBytes.value / 1024).toFixed(1)} / 256 KiB`)
const dirty = computed(() => editorContent.value !== savedEditorContent.value)
const previewHtml = computed(() => buildGuideDocument(editorContent.value).html)
const sortedRevisions = computed(() => [...revisions.value].sort((a, b) => b.version - a.version))

function applyGuideSettings(settings: GuideSettings) {
  version.value = settings.version
  updatedAt.value = settings.updated_at
  hasCustomContent.value = settings.has_custom_content
  revisions.value = settings.revisions ?? []
  editorContent.value = settings.has_custom_content && settings.content.trim()
    ? settings.content
    : bundledGuide
  savedEditorContent.value = editorContent.value
}

function formatDate(value: string): string {
  if (!value) return ''
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return new Intl.DateTimeFormat(locale.value, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(date)
}

async function loadGuide() {
  loading.value = true
  try {
    applyGuideSettings(await adminAPI.settings.getGuideSettings())
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('admin.settings.guideEditor.loadFailed')))
  } finally {
    loading.value = false
  }
}

async function handleMutation(operation: () => Promise<GuideSettings>, successMessage: string) {
  saving.value = true
  try {
    applyGuideSettings(await operation())
    appStore.showSuccess(successMessage)
  } catch (error) {
    if (extractApiErrorCode(error) === 'GUIDE_VERSION_CONFLICT') {
      appStore.showWarning(t('admin.settings.guideEditor.conflict'))
      await loadGuide()
      return
    }
    appStore.showError(extractApiErrorMessage(error, t('admin.settings.guideEditor.saveFailed')))
  } finally {
    saving.value = false
  }
}

async function saveGuide() {
  await handleMutation(
    () => adminAPI.settings.updateGuideSettings({
      content: editorContent.value,
      expected_version: version.value,
    }),
    t('admin.settings.guideEditor.publishSuccess'),
  )
}

async function resetGuide() {
  confirmReset.value = false
  await handleMutation(
    () => adminAPI.settings.resetGuideSettings({ expected_version: version.value }),
    t('admin.settings.guideEditor.resetSuccess'),
  )
}

async function restoreRevision() {
  const revision = pendingRevision.value
  pendingRevision.value = null
  if (!revision) return

  await handleMutation(
    () => adminAPI.settings.restoreGuideSettings({
      revision_version: revision.version,
      expected_version: version.value,
    }),
    t('admin.settings.guideEditor.restoreSuccess'),
  )
}

onMounted(loadGuide)
</script>

<style scoped>
.guide-preview {
  color: inherit;
  line-height: 1.7;
  overflow-wrap: anywhere;
}

.guide-preview :deep(h1) {
  @apply mb-4 text-2xl font-bold text-gray-950 dark:text-white;
}

.guide-preview :deep(h2) {
  @apply mb-3 mt-8 border-b border-gray-200 pb-2 text-xl font-bold text-gray-950 dark:border-dark-700 dark:text-white;
}

.guide-preview :deep(h2:first-child) {
  @apply mt-0;
}

.guide-preview :deep(h3) {
  @apply mb-2 mt-6 text-base font-semibold text-gray-900 dark:text-white;
}

.guide-preview :deep(p) {
  @apply mb-3 text-sm text-gray-700 dark:text-dark-200;
}

.guide-preview :deep(ul) {
  @apply mb-4 list-disc space-y-1 pl-5 text-sm text-gray-700 dark:text-dark-200;
}

.guide-preview :deep(ol) {
  @apply mb-4 list-decimal space-y-1 pl-5 text-sm text-gray-700 dark:text-dark-200;
}

.guide-preview :deep(a) {
  @apply font-medium text-primary-700 underline dark:text-primary-300;
}

.guide-preview :deep(blockquote) {
  @apply my-4 border-l-4 border-amber-400 bg-amber-50 px-3 py-2 text-sm text-amber-950 dark:border-amber-500 dark:bg-amber-500/10 dark:text-amber-100;
}

.guide-preview :deep(blockquote p:last-child) {
  @apply mb-0 text-inherit;
}

.guide-preview :deep(code) {
  @apply rounded bg-gray-100 px-1 py-0.5 font-mono text-[0.875em] text-gray-900 dark:bg-dark-800 dark:text-dark-100;
}

.guide-preview :deep(pre) {
  @apply my-4 max-w-full overflow-x-auto rounded border border-gray-800 bg-gray-950 p-3 text-xs leading-5 text-gray-100;
}

.guide-preview :deep(pre code) {
  @apply whitespace-pre bg-transparent p-0 text-inherit;
}

.guide-preview :deep(table) {
  @apply my-4 block w-full overflow-x-auto border-collapse text-xs;
}

.guide-preview :deep(th) {
  @apply border border-gray-300 bg-gray-50 px-2 py-1.5 text-left font-semibold dark:border-dark-600 dark:bg-dark-800;
}

.guide-preview :deep(td) {
  @apply border border-gray-300 px-2 py-1.5 align-top dark:border-dark-600;
}
</style>
