import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post } = vi.hoisted(() => ({ post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { post } }))

import { setOpenAIAutoResetCreditEnabled } from '@/api/admin/accounts'

describe('admin account automatic reset-credit toggle API', () => {
  beforeEach(() => { post.mockReset() })

  it.each([true, false])('sends enabled=%s without replacing thresholds or unrelated account settings', async enabled => {
    const updated = {
      id: 17,
      extra: {
        auto_reset_credit_enabled: enabled,
        auto_reset_credit_opt_in_version: 1,
        auto_reset_credit_5h_threshold: 0.92,
        auto_reset_credit_7d_threshold: 0.84,
        openai_credits_enabled: false
      }
    }
    post.mockResolvedValue({ data: updated })
    await expect(setOpenAIAutoResetCreditEnabled(17, enabled)).resolves.toBe(updated)
    expect(post.mock.calls).toEqual([['/admin/accounts/17/auto-reset-credit-enabled', { enabled }]])
  })

  it('preserves the API error for the row to display without inventing successful account settings', async () => {
    const error = { status: 409, message: 'An automatic reset is already running' }
    post.mockRejectedValue(error)
    await expect(setOpenAIAutoResetCreditEnabled(17, true)).rejects.toBe(error)
  })
})
