<template>
  <div>
    <label class="input-label">{{ t('admin.accounts.openai.providerPreset.label') }}</label>
    <div
      class="grid grid-cols-2 gap-2 sm:grid-cols-4"
      role="radiogroup"
      :aria-label="t('admin.accounts.openai.providerPreset.label')"
    >
      <button
        v-for="option in options"
        :key="option.id"
        type="button"
        role="radio"
        :aria-checked="modelValue === option.id"
        :data-testid="`openai-provider-preset-${option.id}`"
        :class="[
          'min-h-14 rounded-md border px-3 py-2 text-left transition-colors',
          modelValue === option.id
            ? 'border-primary-500 bg-primary-50 text-primary-700 dark:border-primary-500 dark:bg-primary-900/20 dark:text-primary-300'
            : 'border-gray-200 bg-white text-gray-700 hover:border-gray-300 dark:border-dark-600 dark:bg-dark-700 dark:text-gray-200 dark:hover:border-dark-500'
        ]"
        @click="emit('update:modelValue', option.id)"
      >
        <span class="block text-sm font-medium">{{ option.label }}</span>
        <span v-if="option.models" class="mt-0.5 block break-words font-mono text-[11px] text-gray-500 dark:text-gray-400">
          {{ option.models }}
        </span>
      </button>
    </div>

    <div
      v-if="selectedPreset?.forceNonReasoning"
      class="mt-2 flex items-center gap-2 text-xs font-medium text-amber-700 dark:text-amber-300"
      role="status"
      data-testid="openai-provider-force-non-reasoning"
    >
      <span class="inline-block h-2 w-2 rounded-full bg-amber-500" aria-hidden="true"></span>
      {{ t('admin.accounts.openai.providerPreset.forceNonReasoning') }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  OPENAI_COMPATIBLE_PROVIDER_PRESETS,
  getOpenAICompatibleProviderPreset,
  type OpenAICompatibleProviderSelection
} from './openAICompatibleProviderPresets'

const props = defineProps<{
  modelValue: OpenAICompatibleProviderSelection
}>()

const emit = defineEmits<{
  'update:modelValue': [value: OpenAICompatibleProviderSelection]
}>()

const { t } = useI18n()

const options = computed(() => [
  {
    id: 'custom' as const,
    label: t('admin.accounts.openai.providerPreset.custom'),
    models: ''
  },
  ...OPENAI_COMPATIBLE_PROVIDER_PRESETS.map((preset) => ({
    id: preset.id,
    label: t(`admin.accounts.openai.providerPreset.${preset.labelKey}`),
    models: preset.models.join(', ')
  }))
])

const selectedPreset = computed(() => getOpenAICompatibleProviderPreset(props.modelValue))
</script>
