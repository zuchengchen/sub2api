<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import HelpTooltip from '@/components/common/HelpTooltip.vue'

const props = defineProps<{
  // 当前选中代理已探测到的出口时区（IANA）。为空表示无代理（直连）或尚未探测到。
  proxyTimezone?: string
  // 直连出口探测到的时区；后端按 -1 sentinel 键缓存，管理端暂无对应字段时留空。
  directTimezone?: string
  disabled?: boolean
}>()
const model = defineModel<string>({ default: '' })
const { t } = useI18n()

const override = computed(() => model.value.trim())

// 与后端 OpenAIRequestTimezoneResolver.Resolve 的来源优先级一致：
// override（账号手动覆盖）> proxy / direct（出口缓存）> fallback（可配置默认，默认美西）。
const resolution = computed(() => {
  if (override.value) {
    return { source: 'override' as const, timezone: override.value, fallback: false }
  }
  if (props.proxyTimezone) {
    return { source: 'proxy' as const, timezone: props.proxyTimezone, fallback: false }
  }
  if (props.directTimezone) {
    return { source: 'direct' as const, timezone: props.directTimezone, fallback: false }
  }
  return { source: 'fallback' as const, timezone: 'America/Los_Angeles', fallback: true }
})

const sourceLabel = computed(() => {
  switch (resolution.value.source) {
    case 'override':
      return t('admin.accounts.openai.requestTimezoneSourceOverride')
    case 'proxy':
      return t('admin.accounts.openai.requestTimezoneSourceProxy')
    case 'direct':
      return t('admin.accounts.openai.requestTimezoneSourceDirect')
    default:
      return t('admin.accounts.openai.requestTimezoneSourceFallback')
  }
})
</script>

<template>
  <div>
    <div class="flex items-center">
      <label class="input-label">{{ t('admin.accounts.openai.requestTimezone') }}</label>
      <HelpTooltip width-class="w-80">
        <p>{{ t('admin.accounts.openai.requestTimezoneHelp.intro') }}</p>
        <ul class="mt-2 space-y-1.5">
          <li>{{ t('admin.accounts.openai.requestTimezoneHelp.order') }}</li>
          <li>{{ t('admin.accounts.openai.requestTimezoneHelp.fallback') }}</li>
        </ul>
      </HelpTooltip>
    </div>
    <input
      v-model="model"
      data-testid="openai-request-timezone"
      type="text"
      class="input"
      :disabled="disabled"
      :placeholder="t('admin.accounts.openai.requestTimezonePlaceholder')"
      spellcheck="false"
    />
    <p
      class="mt-1 text-xs"
      :class="resolution.fallback ? 'text-amber-600 dark:text-amber-400' : 'text-gray-500 dark:text-gray-400'"
      data-testid="openai-request-timezone-source"
    >
      {{ t('admin.accounts.openai.requestTimezoneSource', { source: sourceLabel, timezone: resolution.timezone }) }}
    </p>
  </div>
</template>
