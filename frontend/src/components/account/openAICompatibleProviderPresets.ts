import type { OpenAIEndpointCapability, OpenAIResponsesMode } from '@/types'

export const OPENAI_COMPATIBLE_PROVIDER_EXTRA_KEY = 'openai_compatible_provider'

export type OpenAICompatibleProviderPresetID = 'mimo' | 'zhipu_glm' | 'alibaba_qwen'
export type OpenAICompatibleProviderSelection = 'custom' | OpenAICompatibleProviderPresetID

export interface OpenAICompatibleProviderPreset {
  id: OpenAICompatibleProviderPresetID
  labelKey: 'mimo' | 'zhipuGlm' | 'alibabaQwen'
  baseUrl: string
  models: readonly string[]
  responsesMode: OpenAIResponsesMode
  endpointCapabilities: readonly OpenAIEndpointCapability[]
  forceNonReasoning: true
}

export const OPENAI_COMPATIBLE_PROVIDER_PRESETS: readonly OpenAICompatibleProviderPreset[] = [
  {
    id: 'mimo',
    labelKey: 'mimo',
    baseUrl: 'https://api.xiaomimimo.com/v1',
    models: ['mimo-v2.5'],
    responsesMode: 'force_responses',
    endpointCapabilities: ['chat_completions'],
    forceNonReasoning: true
  },
  {
    id: 'zhipu_glm',
    labelKey: 'zhipuGlm',
    baseUrl: 'https://open.bigmodel.cn/api/paas/v4',
    models: ['glm-4.7-flash', 'glm-4.7-flashx'],
    responsesMode: 'force_chat_completions',
    endpointCapabilities: ['chat_completions'],
    forceNonReasoning: true
  },
  {
    id: 'alibaba_qwen',
    labelKey: 'alibabaQwen',
    baseUrl: 'https://dashscope.aliyuncs.com/compatible-mode/v1',
    models: ['qwen3.7-flash'],
    responsesMode: 'force_responses',
    endpointCapabilities: ['chat_completions'],
    forceNonReasoning: true
  }
]

export function getOpenAICompatibleProviderPreset(
  selection: OpenAICompatibleProviderSelection
): OpenAICompatibleProviderPreset | undefined {
  if (selection === 'custom') return undefined
  return OPENAI_COMPATIBLE_PROVIDER_PRESETS.find((preset) => preset.id === selection)
}

export function readOpenAICompatibleProviderSelection(
  extra: Record<string, unknown> | null | undefined
): OpenAICompatibleProviderSelection {
  const value = extra?.[OPENAI_COMPATIBLE_PROVIDER_EXTRA_KEY]
  return OPENAI_COMPATIBLE_PROVIDER_PRESETS.some((preset) => preset.id === value)
    ? (value as OpenAICompatibleProviderPresetID)
    : 'custom'
}

export function buildOpenAICompatibleProviderModelMappings(
  preset: OpenAICompatibleProviderPreset
): Array<{ from: string; to: string }> {
  return preset.models.map((model) => ({ from: model, to: model }))
}

export function applyOpenAICompatibleProviderSelection(
  extra: Record<string, unknown>,
  selection: OpenAICompatibleProviderSelection
): void {
  if (selection === 'custom') {
    delete extra[OPENAI_COMPATIBLE_PROVIDER_EXTRA_KEY]
    return
  }
  extra[OPENAI_COMPATIBLE_PROVIDER_EXTRA_KEY] = selection
}
