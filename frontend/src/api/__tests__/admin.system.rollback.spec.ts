import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, post } = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
}))

vi.mock('../client', () => ({
  apiClient: {
    get,
    post,
  },
}))

import { getRollbackVersions, rollback, type RollbackVersionInfo } from '@/api/admin/system'

describe('admin system rollback API', () => {
  beforeEach(() => {
    get.mockReset()
    post.mockReset()
  })

  it('getRollbackVersions fetches the rollback version list', async () => {
    const versions: RollbackVersionInfo[] = [
      {
        id: 'installed-0.1.146',
        version: '0.1.146',
        commit: 'abc146',
        installed_at: '2026-07-07T00:00:00Z',
        archived_at: '2026-07-08T00:00:00Z',
        sha256: 'f'.repeat(64)
      }
    ]
    get.mockResolvedValue({ data: { versions } })

    const result = await getRollbackVersions()

    expect(get).toHaveBeenCalledWith('/admin/system/rollback-versions')
    expect(result.versions).toEqual(versions)
  })

  it('rollback posts the local history ID in the request body', async () => {
    post.mockResolvedValue({ data: { message: 'ok', need_restart: true } })

    const result = await rollback('installed-0.1.146')

    expect(post).toHaveBeenCalledWith(
      '/admin/system/rollback',
      { id: 'installed-0.1.146' },
      { timeout: 15 * 60 * 1000 }
    )
    expect(result.need_restart).toBe(true)
  })

  it('rollback without a version posts no body (legacy backup rollback)', async () => {
    post.mockResolvedValue({ data: { message: 'ok', need_restart: true } })

    await rollback()

    expect(post).toHaveBeenCalledWith(
      '/admin/system/rollback',
      undefined,
      { timeout: 15 * 60 * 1000 }
    )
  })
})
