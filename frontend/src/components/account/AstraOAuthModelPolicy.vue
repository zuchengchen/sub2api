<template>
  <div data-testid="astra-oauth-model-policy" :data-verdict="verdict" :title="policy?.sampled_at ? t('admin.accounts.openai.astraPolicySampledAt', { time: policy.sampled_at }) : undefined" :class="compact ? 'text-[11px] leading-4' : 'rounded-lg bg-gray-50 p-3 text-sm dark:bg-dark-700'">
    <p class="font-medium">{{ t('admin.accounts.openai.astraPolicyTitle') }}</p>
    <p :class="verdict === 'healthy' ? 'text-emerald-600 dark:text-emerald-400' : verdict === 'degraded' ? 'text-red-600 dark:text-red-400' : 'text-gray-500 dark:text-gray-400'">
      {{ t(`admin.accounts.openai.astraPolicyRange.${verdict}`) }}
    </p>
    <p v-if="!compact" class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.openai.astraPolicyDescription') }}</p>
    <p v-if="policy?.sampled_at" class="mt-1 text-xs text-gray-500 dark:text-gray-400" :class="compact && 'sr-only'">
      {{ t('admin.accounts.openai.astraPolicySampledAt', { time: policy.sampled_at }) }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Account } from '@/types'

const props = withDefaults(defineProps<{
  policy?: Account['openai_oauth_model_policy']
  compact?: boolean
}>(), { compact: false })
const { t } = useI18n()
const verdict = computed(() => props.policy?.verdict ?? 'unknown')
</script>
