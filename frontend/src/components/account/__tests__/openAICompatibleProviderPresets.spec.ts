import { describe, expect, it } from 'vitest'
import {
  OPENAI_COMPATIBLE_PROVIDER_EXTRA_KEY,
  OPENAI_COMPATIBLE_PROVIDER_PRESETS,
  applyOpenAICompatibleProviderSelection,
  buildOpenAICompatibleProviderModelMappings,
  getOpenAICompatibleProviderPreset,
  readOpenAICompatibleProviderSelection
} from '../openAICompatibleProviderPresets'

describe('OpenAI-compatible provider presets', () => {
  it('matches the server-owned provider contract', () => {
    expect(OPENAI_COMPATIBLE_PROVIDER_PRESETS).toEqual([
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
    ])
  })

  it('builds defensive identity mappings for managed models', () => {
    const preset = getOpenAICompatibleProviderPreset('zhipu_glm')
    expect(preset).toBeDefined()
    const mappings = buildOpenAICompatibleProviderModelMappings(preset!)
    expect(mappings).toEqual([
      { from: 'glm-4.7-flash', to: 'glm-4.7-flash' },
      { from: 'glm-4.7-flashx', to: 'glm-4.7-flashx' }
    ])
    mappings[0]!.to = 'changed'
    expect(preset?.models[0]).toBe('glm-4.7-flash')
  })

  it('reads known markers and treats missing or unknown values as custom', () => {
    expect(
      readOpenAICompatibleProviderSelection({
        [OPENAI_COMPATIBLE_PROVIDER_EXTRA_KEY]: 'mimo'
      })
    ).toBe('mimo')
    expect(readOpenAICompatibleProviderSelection({})).toBe('custom')
    expect(
      readOpenAICompatibleProviderSelection({
        [OPENAI_COMPATIBLE_PROVIDER_EXTRA_KEY]: 'unknown'
      })
    ).toBe('custom')
  })

  it('sets or removes only the provider marker', () => {
    const extra: Record<string, unknown> = { keep: true }
    applyOpenAICompatibleProviderSelection(extra, 'alibaba_qwen')
    expect(extra).toEqual({ keep: true, [OPENAI_COMPATIBLE_PROVIDER_EXTRA_KEY]: 'alibaba_qwen' })
    applyOpenAICompatibleProviderSelection(extra, 'custom')
    expect(extra).toEqual({ keep: true })
  })
})
