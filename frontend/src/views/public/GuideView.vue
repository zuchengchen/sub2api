<template>
  <div data-test="guide-view" class="min-h-screen bg-white text-gray-900 dark:bg-dark-950 dark:text-white">
    <header class="sticky top-0 z-40 border-b border-gray-200 bg-white/95 backdrop-blur dark:border-dark-800 dark:bg-dark-950/95">
      <div class="mx-auto flex h-16 max-w-7xl items-center justify-between gap-3 px-4 sm:px-6">
        <RouterLink to="/home" class="flex min-w-0 items-center gap-3" aria-label="返回首页">
          <span class="flex h-9 w-9 shrink-0 items-center justify-center overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900">
            <img :src="siteLogo || '/logo.svg'" alt="" class="h-full w-full object-contain" />
          </span>
          <span class="min-w-0 truncate text-sm font-semibold sm:text-base">{{ siteName }}</span>
          <span class="hidden border-l border-gray-200 pl-3 text-sm text-gray-500 dark:border-dark-700 dark:text-dark-400 sm:inline">使用教程</span>
        </RouterLink>

        <div class="flex shrink-0 items-center gap-1.5 sm:gap-2">
          <a
            data-test="guide-download"
            :href="downloadUrl"
            download
            class="inline-flex h-9 items-center justify-center gap-2 rounded-md border border-gray-300 px-2.5 text-sm font-medium text-gray-700 transition hover:bg-gray-50 dark:border-dark-600 dark:text-dark-200 dark:hover:bg-dark-800 sm:px-3"
            title="下载自动测速脚本"
          >
            <Icon name="download" size="sm" />
            <span class="hidden sm:inline">下载脚本</span>
          </a>
          <button
            type="button"
            class="inline-flex h-9 w-9 items-center justify-center rounded-md text-gray-500 transition hover:bg-gray-100 hover:text-gray-800 dark:text-dark-400 dark:hover:bg-dark-800 dark:hover:text-white"
            :title="isDark ? '切换到浅色模式' : '切换到深色模式'"
            :aria-label="isDark ? '切换到浅色模式' : '切换到深色模式'"
            @click="toggleTheme"
          >
            <Icon :name="isDark ? 'sun' : 'moon'" size="sm" />
          </button>
          <RouterLink
            :to="dashboardPath"
            class="inline-flex h-9 items-center justify-center rounded-md bg-gray-900 px-3 text-sm font-medium text-white transition hover:bg-gray-800 dark:bg-white dark:text-gray-900 dark:hover:bg-gray-200"
          >
            {{ authStore.isAuthenticated ? '控制台' : '登录' }}
          </RouterLink>
        </div>
      </div>
    </header>

    <div class="mx-auto grid max-w-7xl gap-8 px-4 py-8 sm:px-6 lg:grid-cols-[13rem_minmax(0,1fr)] lg:gap-12 lg:py-10">
      <aside class="hidden lg:block" aria-label="教程目录">
        <nav class="sticky top-24 max-h-[calc(100vh-7rem)] overflow-y-auto border-r border-gray-200 pr-5 dark:border-dark-800">
          <p class="mb-3 text-xs font-semibold uppercase text-gray-500 dark:text-dark-400">目录</p>
          <a
            v-for="section in sections"
            :key="section.id"
            :href="`#${section.id}`"
            class="block border-l-2 py-1.5 pl-3 text-sm leading-5 transition"
            :class="activeSection === section.id
              ? 'border-primary-500 font-medium text-primary-700 dark:text-primary-300'
              : 'border-transparent text-gray-600 hover:border-gray-300 hover:text-gray-950 dark:text-dark-300 dark:hover:border-dark-600 dark:hover:text-white'"
            :aria-current="activeSection === section.id ? 'location' : undefined"
            @click="selectSection(section.id)"
          >
            {{ section.label }}
          </a>
        </nav>
      </aside>

      <main class="min-w-0">
        <div class="border-b border-gray-200 pb-7 dark:border-dark-800">
          <p class="text-sm font-semibold text-primary-700 dark:text-primary-300">本站使用手册</p>
          <h1 class="mt-2 text-3xl font-bold text-gray-950 dark:text-white sm:text-4xl">从充值到调用，一步完成配置</h1>
          <p class="mt-4 max-w-3xl text-base leading-7 text-gray-600 dark:text-dark-300">
            面向本站用户的完整操作说明，涵盖兑换余额、API Key、Codex、自动测速脚本与 SVIP 规则。
          </p>
          <div class="mt-4 flex flex-wrap gap-x-5 gap-y-2 text-sm text-gray-500 dark:text-dark-400">
            <span>教程版本 {{ guideVersion }}</span>
            <span>更新日期 {{ updatedAt }}</span>
            <span>适用于当前 Sub2API 站点</span>
          </div>
        </div>

        <div class="border-b border-amber-200 bg-amber-50 px-4 py-3 text-sm leading-6 text-amber-900 dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-100">
          自动测速脚本需要管理员权限并会修改 Windows <code>hosts</code> 与当前 Codex provider 的 <code>base_url</code> 域名。运行前请先关闭 Codex，并阅读脚本章节的备份与恢复说明。
        </div>

        <section class="border-b border-gray-200 py-5 dark:border-dark-800 lg:hidden" aria-label="移动端教程目录">
          <button
            data-test="mobile-toc-toggle"
            type="button"
            class="flex h-10 w-full items-center justify-between rounded-md border border-gray-300 px-3 text-sm font-medium dark:border-dark-600"
            :aria-expanded="mobileTocOpen"
            aria-controls="mobile-guide-toc"
            @click="mobileTocOpen = !mobileTocOpen"
          >
            <span class="flex items-center gap-2"><Icon name="menu" size="sm" />教程目录</span>
            <Icon :name="mobileTocOpen ? 'chevronUp' : 'chevronDown'" size="sm" />
          </button>
          <nav v-if="mobileTocOpen" id="mobile-guide-toc" class="mt-2 border-l border-gray-200 pl-3 dark:border-dark-700">
            <a
              v-for="section in sections"
              :key="section.id"
              :href="`#${section.id}`"
              class="block py-2 text-sm text-gray-700 dark:text-dark-200"
              @click="selectSection(section.id)"
            >
              {{ section.label }}
            </a>
          </nav>
        </section>

        <section v-if="quickCommands.length" class="border-b border-gray-200 py-7 dark:border-dark-800" aria-labelledby="quick-commands-title">
          <div class="mb-4 flex items-center gap-2">
            <Icon name="terminal" size="sm" class="text-primary-600 dark:text-primary-300" />
            <h2 id="quick-commands-title" class="text-lg font-semibold">常用命令</h2>
          </div>
          <div class="divide-y divide-gray-200 border-y border-gray-200 dark:divide-dark-700 dark:border-dark-700">
            <div v-for="command in quickCommands" :key="command.id" class="grid min-w-0 gap-2 py-3 sm:grid-cols-[9rem_minmax(0,1fr)_2.5rem] sm:items-center">
              <span class="text-sm font-medium text-gray-700 dark:text-dark-200">{{ command.label }}</span>
              <code class="min-w-0 overflow-x-auto whitespace-pre rounded-md bg-gray-950 px-3 py-2 text-xs text-gray-100">{{ command.content }}</code>
              <button
                type="button"
                class="inline-flex h-9 w-9 items-center justify-center justify-self-end rounded-md text-gray-500 transition hover:bg-gray-100 hover:text-gray-900 dark:text-dark-400 dark:hover:bg-dark-800 dark:hover:text-white"
                :data-test="`copy-${command.id}`"
                :title="`复制${command.label}`"
                :aria-label="`复制${command.label}`"
                @click="copyCommand(command)"
              >
                <Icon :name="lastCopiedCommand === command.id ? 'check' : 'copy'" size="sm" />
              </button>
            </div>
          </div>
          <p class="sr-only" aria-live="polite">{{ copyAnnouncement }}</p>
        </section>

        <article data-test="guide-content" class="guide-content py-8" v-html="renderedHtml"></article>

        <footer class="mt-4 flex flex-col gap-3 border-t border-gray-200 py-6 text-sm text-gray-500 dark:border-dark-800 dark:text-dark-400 sm:flex-row sm:items-center sm:justify-between">
          <span>教程内容以本站当前页面和实际可用功能为准。</span>
          <RouterLink to="/home" class="inline-flex items-center gap-1.5 font-medium text-primary-700 hover:text-primary-800 dark:text-primary-300 dark:hover:text-primary-200">
            <Icon name="home" size="sm" />返回首页
          </RouterLink>
        </footer>
      </main>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import { useClipboard } from '@/composables/useClipboard'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import { sanitizeUrl } from '@/utils/url'
import {
  buildGuideDocument,
  extractGuideCommands,
  type GuideCommand,
} from '@/utils/guideMarkdown'
import guideMarkdown from '../../../../docs/guide.zh.md?raw'

const downloadUrl = '/downloads/select-fastest-codex-base-url.bat'
const guideVersion = '1.0'
const updatedAt = '2026-08-30'
const commandLabels: Record<string, string> = {
  'skill-install': '安装 goal-workflow',
  'skill-update': '更新 goal-workflow',
  'goal-example': '使用 goal-workflow',
}

const guideDocument = buildGuideDocument(guideMarkdown)
const appStore = useAppStore()
const authStore = useAuthStore()
const { copyToClipboard } = useClipboard()
const isDark = ref(document.documentElement.classList.contains('dark'))
const mobileTocOpen = ref(false)
const activeSection = ref(guideDocument.sections[0]?.id || '')
const lastCopiedCommand = ref('')
const copyAnnouncement = ref('')
let sectionObserver: IntersectionObserver | null = null
let copyTimer: number | undefined

const renderedHtml = guideDocument.html
const sections = guideDocument.sections
const quickCommands = extractGuideCommands(guideMarkdown)
  .filter((command) => commandLabels[command.id])
  .map((command) => ({ ...command, label: commandLabels[command.id] }))
const settings = computed(() => appStore.cachedPublicSettings)
const siteName = computed(() => settings.value?.site_name || appStore.siteName || 'Sub2API')
const siteLogo = computed(() => sanitizeUrl(
  settings.value?.site_logo || appStore.siteLogo || '',
  { allowRelative: true, allowDataUrl: true },
))
const dashboardPath = computed(() => {
  if (!authStore.isAuthenticated) return '/login'
  return authStore.isAdmin ? '/admin/dashboard' : '/dashboard'
})

function toggleTheme() {
  isDark.value = !isDark.value
  document.documentElement.classList.toggle('dark', isDark.value)
  localStorage.setItem('theme', isDark.value ? 'dark' : 'light')
}

function selectSection(sectionId: string) {
  activeSection.value = sectionId
  mobileTocOpen.value = false
}

async function copyCommand(command: GuideCommand) {
  const success = await copyToClipboard(command.content, '命令已复制')
  copyAnnouncement.value = success ? `${commandLabels[command.id]}已复制` : '复制失败'
  lastCopiedCommand.value = success ? command.id : ''

  if (copyTimer !== undefined) window.clearTimeout(copyTimer)
  copyTimer = window.setTimeout(() => {
    lastCopiedCommand.value = ''
    copyAnnouncement.value = ''
  }, 2000)
}

onMounted(async () => {
  if (!appStore.publicSettingsLoaded) {
    void appStore.fetchPublicSettings()
  }

  await nextTick()
  sectionObserver = new IntersectionObserver((entries) => {
    const visible = entries.find((entry) => entry.isIntersecting)
    if (visible?.target.id) activeSection.value = visible.target.id
  }, { rootMargin: '-80px 0px -70% 0px' })

  sections.forEach((section) => {
    const heading = document.getElementById(section.id)
    if (heading) sectionObserver?.observe(heading)
  })

  if (window.location.hash) {
    try {
      const sectionId = decodeURIComponent(window.location.hash.slice(1))
      if (sections.some((section) => section.id === sectionId)) {
        activeSection.value = sectionId
        window.requestAnimationFrame(() => document.getElementById(sectionId)?.scrollIntoView())
      }
    } catch {
      // Ignore malformed hashes and keep the first section active.
    }
  }
})

onBeforeUnmount(() => {
  sectionObserver?.disconnect()
  if (copyTimer !== undefined) window.clearTimeout(copyTimer)
})
</script>

<style scoped>
.guide-content {
  color: inherit;
  line-height: 1.75;
  overflow-wrap: anywhere;
}

.guide-content :deep(h2) {
  @apply mb-4 mt-12 border-b border-gray-200 pb-3 text-2xl font-bold text-gray-950 dark:border-dark-700 dark:text-white;
  scroll-margin-top: 5.5rem;
}

.guide-content :deep(h2:first-child) {
  @apply mt-0;
}

.guide-content :deep(h3) {
  @apply mb-3 mt-8 text-lg font-semibold text-gray-900 dark:text-white;
  scroll-margin-top: 5.5rem;
}

.guide-content :deep(p) {
  @apply mb-4 text-gray-700 dark:text-dark-200;
}

.guide-content :deep(ul) {
  @apply mb-5 list-disc space-y-1.5 pl-6 text-gray-700 dark:text-dark-200;
}

.guide-content :deep(ol) {
  @apply mb-5 list-decimal space-y-2 pl-6 text-gray-700 dark:text-dark-200;
}

.guide-content :deep(li > ul),
.guide-content :deep(li > ol) {
  @apply mb-2 mt-2;
}

.guide-content :deep(a) {
  @apply font-medium text-primary-700 underline decoration-primary-300 underline-offset-4 hover:text-primary-800 dark:text-primary-300 dark:decoration-primary-700 dark:hover:text-primary-200;
}

.guide-content :deep(blockquote) {
  @apply my-6 border-l-4 border-amber-400 bg-amber-50 px-4 py-3 text-amber-950 dark:border-amber-500 dark:bg-amber-500/10 dark:text-amber-100;
}

.guide-content :deep(blockquote p:last-child) {
  @apply mb-0 text-inherit;
}

.guide-content :deep(code) {
  @apply rounded bg-gray-100 px-1.5 py-0.5 font-mono text-[0.875em] text-gray-900 dark:bg-dark-800 dark:text-dark-100;
}

.guide-content :deep(pre) {
  @apply my-5 max-w-full overflow-x-auto rounded-lg border border-gray-800 bg-gray-950 p-4 text-sm leading-6 text-gray-100;
}

.guide-content :deep(pre code) {
  @apply whitespace-pre bg-transparent p-0 text-inherit;
}

.guide-content :deep(table) {
  @apply my-6 block w-full overflow-x-auto border-collapse text-sm;
}

.guide-content :deep(th) {
  @apply whitespace-nowrap border border-gray-300 bg-gray-50 px-3 py-2 text-left font-semibold text-gray-900 dark:border-dark-600 dark:bg-dark-800 dark:text-white;
}

.guide-content :deep(td) {
  @apply border border-gray-300 px-3 py-2 align-top text-gray-700 dark:border-dark-600 dark:text-dark-200;
}

.guide-content :deep(hr) {
  @apply my-8 border-gray-200 dark:border-dark-700;
}

.guide-content :deep(strong) {
  @apply font-semibold text-gray-950 dark:text-white;
}
</style>
