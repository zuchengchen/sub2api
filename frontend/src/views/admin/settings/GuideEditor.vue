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

        <div class="grid min-w-0 gap-5 lg:grid-cols-[16rem_minmax(0,1fr)]">
          <section class="min-w-0" aria-labelledby="guide-chapter-list-label">
            <div class="mb-2 flex items-center justify-between gap-2">
              <h3 id="guide-chapter-list-label" class="text-sm font-semibold text-gray-800 dark:text-gray-200">
                {{ t('admin.settings.guideEditor.chapters') }}
              </h3>
              <span class="text-xs text-gray-500 dark:text-gray-400">
                {{ t('admin.settings.guideEditor.chapterCount', { count: chapters.length }) }}
              </span>
            </div>

            <ul class="divide-y divide-gray-200 border-y border-gray-200 dark:divide-dark-700 dark:border-dark-700" data-test="guide-chapter-list">
              <li
                v-for="(chapter, index) in chapters"
                :key="chapter.slug"
                class="flex items-center gap-1 py-1.5"
                :draggable="true"
                @dragstart="startChapterDrag(index)"
                @dragover.prevent
                @drop.prevent="dropChapter(index)"
                @dragend="draggingIndex = null"
              >
                <button
                  type="button"
                  class="min-w-0 flex-1 truncate rounded px-2 py-1.5 text-left text-sm transition"
                  :class="chapter.slug === activeSlug
                    ? 'bg-primary-50 font-medium text-primary-700 dark:bg-primary-500/10 dark:text-primary-300'
                    : 'text-gray-700 hover:bg-gray-50 dark:text-dark-200 dark:hover:bg-dark-800'"
                  :data-test="`guide-chapter-${chapter.slug}`"
                  :title="chapter.title || chapter.slug"
                  @click="selectChapter(chapter.slug)"
                >
                  <span class="block truncate">{{ chapter.title || chapter.slug }}</span>
                  <span class="block truncate text-xs text-gray-500 dark:text-dark-400">#{{ chapter.slug }}</span>
                </button>
                <span class="flex shrink-0 flex-col">
                  <button
                    type="button"
                    class="flex h-5 w-6 items-center justify-center rounded text-gray-400 transition hover:bg-gray-100 hover:text-gray-700 disabled:opacity-30 dark:hover:bg-dark-800 dark:hover:text-white"
                    :disabled="index === 0"
                    :aria-label="t('admin.settings.guideEditor.moveUp')"
                    :data-test="`guide-chapter-up-${chapter.slug}`"
                    @click="moveChapter(index, -1)"
                  >
                    <Icon name="chevronUp" size="xs" />
                  </button>
                  <button
                    type="button"
                    class="flex h-5 w-6 items-center justify-center rounded text-gray-400 transition hover:bg-gray-100 hover:text-gray-700 disabled:opacity-30 dark:hover:bg-dark-800 dark:hover:text-white"
                    :disabled="index === chapters.length - 1"
                    :aria-label="t('admin.settings.guideEditor.moveDown')"
                    :data-test="`guide-chapter-down-${chapter.slug}`"
                    @click="moveChapter(index, 1)"
                  >
                    <Icon name="chevronDown" size="xs" />
                  </button>
                </span>
              </li>
            </ul>

            <div class="mt-3 space-y-2">
              <div class="flex gap-2">
                <input
                  v-model="newChapterTitle"
                  class="input min-w-0 flex-1 text-sm"
                  :placeholder="t('admin.settings.guideEditor.newChapterTitle')"
                  data-test="guide-chapter-new-title"
                  @keydown.enter.prevent="addChapter"
                />
                <button
                  type="button"
                  class="btn btn-secondary btn-sm shrink-0"
                  :disabled="!newChapterTitle.trim()"
                  data-test="guide-chapter-add"
                  @click="addChapter"
                >
                  <Icon name="plus" size="sm" />
                  {{ t('admin.settings.guideEditor.addChapter') }}
                </button>
              </div>
              <button
                v-if="missingBundledChapters.length"
                type="button"
                class="btn btn-secondary btn-sm w-full"
                data-test="guide-chapter-import-bundled"
                @click="importMissingBundledChapters"
              >
                <Icon name="download" size="sm" />
                {{ t('admin.settings.guideEditor.importBundled', { count: missingBundledChapters.length }) }}
              </button>
            </div>
          </section>

          <section class="min-w-0" aria-labelledby="guide-markdown-label">
            <div class="mb-2 flex flex-wrap items-center justify-between gap-3">
              <label id="guide-markdown-label" for="guide-markdown" class="text-sm font-semibold text-gray-800 dark:text-gray-200">
                {{ activeChapter
                  ? t('admin.settings.guideEditor.editingChapter', { title: activeChapter.title || activeChapter.slug })
                  : t('admin.settings.guideEditor.markdown') }}
              </label>
              <span class="flex items-center gap-3 text-xs">
                <span :class="chapterTooLarge ? 'text-red-600 dark:text-red-400' : 'text-gray-500 dark:text-gray-400'">
                  {{ t('admin.settings.guideEditor.size', { size: formattedSize }) }}
                </span>
                <button
                  v-if="activeChapter"
                  type="button"
                  class="font-medium text-red-600 transition hover:text-red-700 disabled:opacity-40 dark:text-red-400"
                  :disabled="chapters.length <= 1"
                  data-test="guide-chapter-delete"
                  @click="pendingDeleteSlug = activeChapter.slug"
                >
                  {{ t('admin.settings.guideEditor.deleteChapter') }}
                </button>
              </span>
            </div>
            <textarea
              id="guide-markdown"
              ref="markdownTextarea"
              v-model="activeChapterContent"
              class="input min-h-[36rem] resize-y whitespace-pre-wrap font-mono text-sm leading-6"
              :placeholder="t('admin.settings.guideEditor.placeholder')"
              spellcheck="false"
              data-test="guide-markdown"
              @paste="handlePaste"
              @dragover.prevent
              @drop.prevent="handleDrop"
            ></textarea>
            <p v-if="activeChapter" class="mt-2 text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.settings.guideEditor.anchorHint', { slug: activeChapter.slug }) }}
            </p>

            <section class="mt-4 border-t border-gray-200 pt-4 dark:border-dark-700" aria-labelledby="guide-assets-label">
              <div class="mb-2 flex flex-wrap items-center justify-between gap-2">
                <h3 id="guide-assets-label" class="text-sm font-semibold text-gray-800 dark:text-gray-200">
                  {{ t('admin.settings.guideEditor.assets') }}
                </h3>
                <span class="text-xs text-gray-500 dark:text-gray-400">
                  {{ t('admin.settings.guideEditor.assetQuota', { used: formattedAssetUsage }) }}
                </span>
              </div>

              <div class="flex flex-wrap items-center gap-2">
                <input
                  ref="assetInput"
                  type="file"
                  multiple
                  class="hidden"
                  data-test="guide-asset-input"
                  @change="handleAssetInput"
                />
                <button
                  type="button"
                  class="btn btn-secondary btn-sm"
                  :disabled="uploading || !activeChapter"
                  data-test="guide-asset-upload"
                  @click="assetInput?.click()"
                >
                  <span v-if="uploading" class="h-4 w-4 animate-spin rounded-full border-2 border-gray-300 border-t-primary-600"></span>
                  <Icon v-else name="upload" size="sm" />
                  {{ uploading ? t('admin.settings.guideEditor.assetUploading') : t('admin.settings.guideEditor.assetUpload') }}
                </button>
                <span class="text-xs text-gray-500 dark:text-gray-400">
                  {{ t('admin.settings.guideEditor.assetHint') }}
                </span>
              </div>

              <ul
                v-if="assets.length"
                class="mt-3 divide-y divide-gray-200 border-y border-gray-200 dark:divide-dark-700 dark:border-dark-700"
                data-test="guide-asset-list"
              >
                <li v-for="asset in assets" :key="asset.id" class="flex items-center gap-2 py-2">
                  <Icon name="document" size="sm" class="shrink-0 text-gray-400" />
                  <span class="min-w-0 flex-1 truncate text-sm text-gray-700 dark:text-dark-200" :title="asset.name">
                    {{ asset.name }}
                  </span>
                  <span class="shrink-0 text-xs text-gray-500 dark:text-dark-400">{{ formatBytes(asset.size) }}</span>
                  <button
                    type="button"
                    class="shrink-0 rounded px-2 py-1 text-xs font-medium text-primary-700 transition hover:bg-primary-50 dark:text-primary-300 dark:hover:bg-primary-500/10"
                    :disabled="!activeChapter"
                    :data-test="`guide-asset-insert-${asset.id}`"
                    @click="insertAssetReference(asset)"
                  >
                    {{ t('admin.settings.guideEditor.assetInsert') }}
                  </button>
                  <button
                    type="button"
                    class="shrink-0 rounded px-2 py-1 text-xs text-gray-500 transition hover:bg-gray-100 hover:text-gray-800 dark:hover:bg-dark-800 dark:hover:text-white"
                    :data-test="`guide-asset-copy-${asset.id}`"
                    @click="copyAssetUrl(asset)"
                  >
                    {{ t('admin.settings.guideEditor.assetCopy') }}
                  </button>
                  <button
                    type="button"
                    class="shrink-0 rounded px-2 py-1 text-xs text-red-600 transition hover:bg-red-50 dark:text-red-400 dark:hover:bg-red-500/10"
                    :data-test="`guide-asset-delete-${asset.id}`"
                    @click="pendingDeleteAsset = asset"
                  >
                    {{ t('admin.settings.guideEditor.assetDelete') }}
                  </button>
                </li>
              </ul>
              <p v-else class="mt-3 text-xs text-gray-500 dark:text-gray-400">
                {{ t('admin.settings.guideEditor.assetEmpty') }}
              </p>
            </section>
          </section>

        </div>

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
            class="guide-preview max-h-[40rem] min-h-[20rem] overflow-y-auto border border-gray-200 bg-white p-5 dark:border-dark-700 dark:bg-dark-900"
            data-test="guide-preview"
            v-html="previewHtml"
          ></article>
        </section>

        <p v-if="chapterTooLarge" class="text-sm text-red-600 dark:text-red-400">
          {{ t('admin.settings.guideEditor.chapterTooLarge') }}
        </p>
        <p v-else-if="contentTooLarge" class="text-sm text-red-600 dark:text-red-400">
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
              :disabled="saving || !dirty || !chapters.length || chapterTooLarge || contentTooLarge"
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
          data-test="guide-revision-row"
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
                {{ revisionHasContent(revision) ? t('admin.settings.guideEditor.databaseContent') : t('admin.settings.guideEditor.bundledContent') }}
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
    <ConfirmDialog
      :show="pendingDeleteSlug !== ''"
      :title="t('admin.settings.guideEditor.deleteChapterTitle')"
      :message="t('admin.settings.guideEditor.deleteChapterMessage', { slug: pendingDeleteSlug })"
      :confirm-text="t('admin.settings.guideEditor.deleteChapter')"
      @confirm="deleteActiveChapter"
      @cancel="pendingDeleteSlug = ''"
    />
    <ConfirmDialog
      :show="pendingDeleteAsset !== null"
      :title="t('admin.settings.guideEditor.assetDeleteTitle')"
      :message="t('admin.settings.guideEditor.assetDeleteMessage', { name: pendingDeleteAsset?.name ?? '' })"
      :confirm-text="t('admin.settings.guideEditor.assetDelete')"
      @confirm="deletePendingAsset"
      @cancel="pendingDeleteAsset = null"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api'
import type { GuideAsset, GuideChapterPayload, GuideRevision, GuideSettings } from '@/api/admin/settings'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { useClipboard } from '@/composables/useClipboard'
import { useAppStore } from '@/stores'
import { extractApiErrorCode, extractApiErrorMessage } from '@/utils/apiError'
import {
  buildGuideDocumentFromChapters,
  deriveChapterSlug,
  readChapterTitle,
  splitGuideIntoChapters,
} from '@/utils/guideMarkdown'
import { getBundledGuideChapters } from '@/utils/guideSections'

const maxContentBytes = 256 * 1024
const maxChapterBytes = 64 * 1024
const { t, locale } = useI18n()
const appStore = useAppStore()
const { copyToClipboard } = useClipboard()
const loading = ref(true)
const saving = ref(false)
const chapters = ref<GuideChapterPayload[]>([])
const savedChaptersJson = ref('')
const activeSlug = ref('')
const newChapterTitle = ref('')
const draggingIndex = ref<number | null>(null)
const pendingDeleteSlug = ref('')
const version = ref(0)
const updatedAt = ref('')
const hasCustomContent = ref(false)
const revisions = ref<GuideRevision[]>([])
const confirmReset = ref(false)
const pendingRevision = ref<GuideRevision | null>(null)

const assets = ref<GuideAsset[]>([])
const assetInput = ref<HTMLInputElement | null>(null)
const markdownTextarea = ref<HTMLTextAreaElement | null>(null)
const uploading = ref(false)
const pendingDeleteAsset = ref<GuideAsset | null>(null)

const activeChapter = computed(() => chapters.value.find((chapter) => chapter.slug === activeSlug.value) || null)
const activeChapterContent = computed({
  get: () => activeChapter.value?.content || '',
  set: (value: string) => {
    const chapter = activeChapter.value
    if (!chapter) return
    chapter.content = value
    // The `##` heading is the chapter title, so keep the list label in step.
    chapter.title = readChapterTitle(value) || chapter.title
  },
})

const chapterBytes = computed(() => new TextEncoder().encode(activeChapterContent.value).byteLength)
const totalBytes = computed(() => chapters.value.reduce(
  (sum, chapter) => sum + new TextEncoder().encode(chapter.content).byteLength,
  0,
))
const chapterTooLarge = computed(() => chapterBytes.value > maxChapterBytes)
const contentTooLarge = computed(() => totalBytes.value > maxContentBytes)
const formattedSize = computed(() => `${(chapterBytes.value / 1024).toFixed(1)} / 64 KiB`)
const dirty = computed(() => JSON.stringify(chapters.value) !== savedChaptersJson.value)
const previewHtml = computed(() => buildGuideDocumentFromChapters(chapters.value).html)
const sortedRevisions = computed(() => [...revisions.value].sort((a, b) => b.version - a.version))
const missingBundledChapters = computed(() => {
  const present = new Set(chapters.value.map((chapter) => chapter.slug))
  return getBundledGuideChapters().filter((chapter) => !present.has(chapter.slug))
})
const assetUsedBytes = computed(() => assets.value.reduce((sum, asset) => sum + asset.size, 0))
const formattedAssetUsage = computed(() => `${formatBytes(assetUsedBytes.value)} / 2 GiB`)

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MiB`
  return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GiB`
}

/**
 * Builds the Markdown for an asset: an image renders inline, anything else
 * becomes a download link. The alt/link text uses the original file name.
 */
function assetMarkdown(asset: GuideAsset): string {
  const label = asset.name.replace(/[[\]]/g, '')
  return asset.inline ? `![${label}](${asset.url})` : `[${label}](${asset.url})`
}

/** Inserts text at the cursor, or appends when the textarea is not focused. */
function insertAtCursor(snippet: string) {
  const chapter = activeChapter.value
  if (!chapter) return

  const textarea = markdownTextarea.value
  const content = chapter.content
  if (!textarea) {
    activeChapterContent.value = `${content.replace(/\s*$/, '')}\n\n${snippet}\n`
    return
  }

  const start = textarea.selectionStart ?? content.length
  const end = textarea.selectionEnd ?? start
  activeChapterContent.value = content.slice(0, start) + snippet + content.slice(end)

  // Restore the caret after the inserted text once Vue has re-rendered.
  const caret = start + snippet.length
  void nextTick(() => {
    textarea.focus()
    textarea.setSelectionRange(caret, caret)
  })
}

function insertAssetReference(asset: GuideAsset) {
  insertAtCursor(assetMarkdown(asset))
}

async function copyAssetUrl(asset: GuideAsset) {
  const absolute = new URL(asset.url, window.location.origin).toString()
  const ok = await copyToClipboard(absolute, t('admin.settings.guideEditor.assetCopied'))
  if (!ok) appStore.showError(t('admin.settings.guideEditor.assetCopyFailed'))
}

async function loadAssets() {
  try {
    assets.value = await adminAPI.settings.listGuideAssets()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('admin.settings.guideEditor.assetLoadFailed')))
  }
}

/**
 * Uploads files and inserts a reference for each. Uploads run sequentially so
 * the quota check on the server sees each file's real remaining budget.
 */
async function uploadFiles(files: File[]) {
  if (!files.length) return
  if (!activeChapter.value) {
    appStore.showError(t('admin.settings.guideEditor.assetNeedChapter'))
    return
  }

  uploading.value = true
  try {
    for (const file of files) {
      try {
        const asset = await adminAPI.settings.uploadGuideAsset(file)
        assets.value = [asset, ...assets.value]
        insertAtCursor(assetMarkdown(asset))
      } catch (error) {
        appStore.showError(extractApiErrorMessage(
          error,
          t('admin.settings.guideEditor.assetUploadFailed', { name: file.name }),
        ))
      }
    }
  } finally {
    uploading.value = false
  }
}

async function handleAssetInput(event: Event) {
  const input = event.target as HTMLInputElement
  const files = Array.from(input.files || [])
  // Reset so selecting the same file again still fires a change event.
  input.value = ''
  await uploadFiles(files)
}

async function handleDrop(event: DragEvent) {
  const files = Array.from(event.dataTransfer?.files || [])
  if (files.length) await uploadFiles(files)
}

async function handlePaste(event: ClipboardEvent) {
  const files = Array.from(event.clipboardData?.items || [])
    .filter((item) => item.kind === 'file')
    .map((item) => item.getAsFile())
    .filter((file): file is File => file !== null)

  // Only intercept the paste when it actually carries files, so pasting text
  // keeps its normal behaviour.
  if (!files.length) return
  event.preventDefault()
  await uploadFiles(files)
}

async function deletePendingAsset() {
  const asset = pendingDeleteAsset.value
  pendingDeleteAsset.value = null
  if (!asset) return

  try {
    await adminAPI.settings.deleteGuideAsset(asset.id)
    assets.value = assets.value.filter((item) => item.id !== asset.id)
    appStore.showSuccess(t('admin.settings.guideEditor.assetDeleted'))
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, t('admin.settings.guideEditor.assetDeleteFailed')))
  }
}

/**
 * Resolves the chapter list to edit: the published chapters when present, a
 * published single document split into chapters, otherwise the bundled guide.
 */
function resolveEditableChapters(settings: GuideSettings): GuideChapterPayload[] {
  if (settings.has_custom_content) {
    if (settings.chapters?.length) {
      return settings.chapters.map((chapter) => ({ ...chapter }))
    }
    if (settings.content?.trim()) {
      return splitGuideIntoChapters(settings.content)
    }
  }
  return getBundledGuideChapters()
}

function applyGuideSettings(settings: GuideSettings) {
  version.value = settings.version
  updatedAt.value = settings.updated_at
  hasCustomContent.value = settings.has_custom_content
  revisions.value = settings.revisions ?? []
  chapters.value = resolveEditableChapters(settings)
  savedChaptersJson.value = JSON.stringify(chapters.value)

  if (!chapters.value.some((chapter) => chapter.slug === activeSlug.value)) {
    activeSlug.value = chapters.value[0]?.slug || ''
  }
}

/**
 * A revision holds chapters when it was published by the chapter editor and a
 * single `content` document when it predates chapters. Either form counts as
 * published content; a revision with neither recorded the bundled guide.
 */
function revisionHasContent(revision: GuideRevision): boolean {
  return Boolean(revision.chapters?.length) || Boolean(revision.content?.trim())
}

function selectChapter(slug: string) {
  activeSlug.value = slug
}

function moveChapter(index: number, delta: number) {
  const target = index + delta
  if (target < 0 || target >= chapters.value.length) return
  const next = [...chapters.value]
  const [moved] = next.splice(index, 1)
  next.splice(target, 0, moved)
  chapters.value = next
}

function startChapterDrag(index: number) {
  draggingIndex.value = index
}

function dropChapter(index: number) {
  const from = draggingIndex.value
  draggingIndex.value = null
  if (from === null || from === index) return
  moveChapter(from, index - from)
}

function addChapter() {
  const title = newChapterTitle.value.trim()
  if (!title) return

  const slug = deriveChapterSlug(title, chapters.value.map((chapter) => chapter.slug))
  chapters.value = [...chapters.value, { slug, title, content: `## ${title}\n\n` }]
  activeSlug.value = slug
  newChapterTitle.value = ''
}

function deleteActiveChapter() {
  const slug = pendingDeleteSlug.value
  pendingDeleteSlug.value = ''
  if (!slug || chapters.value.length <= 1) return

  const index = chapters.value.findIndex((chapter) => chapter.slug === slug)
  chapters.value = chapters.value.filter((chapter) => chapter.slug !== slug)
  if (activeSlug.value === slug) {
    const fallback = chapters.value[Math.min(index, chapters.value.length - 1)]
    activeSlug.value = fallback?.slug || ''
  }
}

function importMissingBundledChapters() {
  const missing = missingBundledChapters.value
  if (!missing.length) return
  chapters.value = [...chapters.value, ...missing.map((chapter) => ({ ...chapter }))]
  activeSlug.value = missing[0].slug
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
      chapters: chapters.value.map((chapter) => ({ ...chapter })),
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

onMounted(async () => {
  await loadGuide()
  await loadAssets()
})
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

.guide-preview :deep(img) {
  @apply my-3 h-auto max-w-full rounded border border-gray-200 dark:border-dark-700;
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
