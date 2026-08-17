import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, put, post } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
  post: vi.fn(),
}))

vi.mock('../client', () => ({
  apiClient: { get, put, post, delete: vi.fn() },
}))

import { getConfig, testDeepSeekChannel, updateConfig } from '@/api/admin/riskControl'

describe('admin risk-control DeepSeek API', () => {
  beforeEach(() => {
    get.mockReset()
    put.mockReset()
    post.mockReset()
  })

  it('loads the unified risk-control configuration', async () => {
    get.mockResolvedValue({ data: { enabled: true, mode: 'pre_block', deepseek_channels: [] } })

    await getConfig()

    expect(get).toHaveBeenCalledWith('/admin/risk-control/config')
  })

  it('sends ordered channels without retired top-level reviewer fields', async () => {
    const payload = {
      deepseek_enabled: true,
      yufeng_enabled: false,
      deepseek_channels: [
        {
          id: 'deepseek-official',
          name: 'DeepSeek Official',
          base_url: 'https://api.deepseek.com',
          model: 'deepseek-v4-flash',
          enabled: true,
          order: 0,
          timeout_ms: 3000,
          api_key: 'temporary-test-value',
        },
      ],
    }
    put.mockResolvedValue({ data: { enabled: true, mode: 'pre_block' } })

    await updateConfig(payload)

    expect(put).toHaveBeenCalledWith('/admin/risk-control/config', payload)
    expect(payload).not.toHaveProperty('base_url')
    expect(payload).not.toHaveProperty('model')
    expect(payload).not.toHaveProperty('api_keys')
  })

  it('runs the saved channel contract test with an empty request body', async () => {
    post.mockResolvedValue({
      data: {
        channel_id: 'official/channel',
        safe_case: { passed: true },
        risk_case: { passed: true },
        health_valid: true,
      },
    })

    await testDeepSeekChannel('official/channel')

    expect(post).toHaveBeenCalledWith('/admin/risk-control/deepseek/channels/official%2Fchannel/test')
  })
})
