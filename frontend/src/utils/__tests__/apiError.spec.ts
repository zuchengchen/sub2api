import { beforeEach, describe, expect, it, vi } from 'vitest'

const t = vi.fn((key: string) => {
  const messages: Record<string, string> = {
    'common.apiErrors.INSUFFICIENT_BALANCE': '余额不足',
    'common.apiErrors.REDEEM_CODE_USED': '兑换码已使用',
  }
  return messages[key] ?? key
})
const te = vi.fn((key: string) => t(key) !== key)

vi.mock('@/i18n', () => ({
  i18n: {
    global: {
      t: (key: string) => t(key),
      te: (key: string) => te(key),
    },
  },
}))

describe('extractApiErrorMessage', () => {
  beforeEach(() => {
    t.mockClear()
    te.mockClear()
  })

  it('uses the selected locale for known API error codes', async () => {
    const { extractApiErrorMessage } = await import('@/utils/apiError')
    expect(extractApiErrorMessage({ code: 'INSUFFICIENT_BALANCE', message: '余额不足 / insufficient balance' })).toBe(
      '余额不足',
    )
  })

  it('prefers reason over numeric HTTP code', async () => {
    const { extractApiErrorMessage } = await import('@/utils/apiError')
    expect(
      extractApiErrorMessage({
        code: 409,
        reason: 'REDEEM_CODE_USED',
        message: '兑换码已使用 / redeem code already used',
      }),
    ).toBe('兑换码已使用')
  })

  it('falls back to the backend message when no locale mapping exists', async () => {
    const { extractApiErrorMessage } = await import('@/utils/apiError')
    expect(extractApiErrorMessage({ code: 'SOME_UNKNOWN_CODE', message: 'backend text' }, 'fallback')).toBe(
      'backend text',
    )
  })
})
