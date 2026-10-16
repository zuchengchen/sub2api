<template>
  <div class="flex items-center gap-1" :class="compact ? 'text-[10px] leading-4' : 'text-sm'">
    <span class="truncate font-medium text-gray-500 dark:text-gray-400" :title="model">{{ shortName }}</span>
    <span
      class="inline-flex shrink-0 items-center"
      :class="compact ? 'gap-px' : 'gap-0.5'"
      :aria-label="sparkLabel"
    >
      <span
        v-for="(slot, index) in slots"
        :key="index"
        class="inline-block rounded-full"
        data-testid="tibo-dot"
        :data-verdict="slot?.verdict || 'empty'"
        :class="[compact ? 'h-1.5 w-1.5' : 'h-2 w-2', slotClass(slot)]"
        :title="slotTitle(slot)"
      />
    </span>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { TiboRouteVerdict } from '@/types'

const props = withDefaults(defineProps<{
  model: string
  verdict: string
  samples?: { verdict: TiboRouteVerdict; at?: string }[]
  compact?: boolean
}>(), {
  samples: () => [],
  compact: true,
})

const { t } = useI18n()
const sampleLimit = 10

const shortName = computed(() => {
  if (props.model === 'gpt-6-astra') return 'astra'
  if (props.model === 'gpt-5.6-sol') return 'sol'
  return props.model
})

const slots = computed(() => {
  const samples = (props.samples ?? []).filter((sample) => sample.verdict === 'healthy' || sample.verdict === 'degraded')
  const missing = Math.max(0, sampleLimit - samples.length)
  return [
    ...Array.from({ length: missing }, () => null),
    ...samples.slice(-sampleLimit),
  ]
})

const sparkLabel = computed(() => t('admin.accounts.openai.codexTiboRecentSamples'))

function slotClass(slot: { verdict: TiboRouteVerdict; at?: string } | null) {
  if (!slot) return 'bg-gray-200 dark:bg-gray-700'
  if (slot.verdict === 'healthy') return 'bg-emerald-500'
  return 'bg-red-500'
}

function slotTitle(slot: { verdict: TiboRouteVerdict; at?: string } | null) {
  if (!slot) return t('admin.accounts.openai.codexTiboSampleEmpty')
  const label = t(`admin.accounts.openai.codexTiboRouteVerdict.${slot.verdict}`)
  return slot.at ? `${label} · ${slot.at}` : label
}
</script>
