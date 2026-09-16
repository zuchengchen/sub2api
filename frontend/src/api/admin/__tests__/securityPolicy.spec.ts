import { beforeEach, describe, expect, it, vi } from 'vitest'
import { securityPolicyAPI } from '../securityPolicy'

const get = vi.fn()
const post = vi.fn()
vi.mock('../../client', () => ({
  apiClient: {
    get: (...args: unknown[]) => get(...args),
    post: (...args: unknown[]) => post(...args),
    put: vi.fn(),
    delete: vi.fn()
  }
}))

describe('security policy admin API', () => {
  beforeEach(() => {
    get.mockReset()
    post.mockReset()
  })

  it('lists builtin keywords and keeps the group switch off by default in payloads', async () => {
    get.mockResolvedValue({ data: { keywords: [{ keyword: '破解教程', category: 'crack' }], count: 1 } })
    const result = await securityPolicyAPI.listBuiltin()
    expect(get).toHaveBeenCalledWith('/admin/security-policy/keywords/builtin')
    expect(result.keywords[0].keyword).toBe('破解教程')
    expect(result.keywords[0].category).toBe('crack')
  })

  it('unblocks a session without promoting roles', async () => {
    post.mockResolvedValue({ data: { unblocked: true } })
    const result = await securityPolicyAPI.unblockSession({ group_id: 9, api_key_id: 11 })
    expect(post).toHaveBeenCalledWith('/admin/security-policy/sessions/unblock', {
      group_id: 9,
      api_key_id: 11
    })
    expect(result.unblocked).toBe(true)
  })
})
