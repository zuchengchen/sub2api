import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import UserApiKeysModal from './UserApiKeysModal.vue'

const { getUserApiKeys, getAllGroups, updateApiKey } = vi.hoisted(() => ({
  getUserApiKeys: vi.fn(),
  getAllGroups: vi.fn(),
  updateApiKey: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    users: { getUserApiKeys },
    groups: { getAll: getAllGroups },
    apiKeys: { updateApiKey, updateApiKeyGroup: vi.fn() },
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

describe('UserApiKeysModal concurrency field', () => {
  beforeEach(() => {
    getUserApiKeys.mockResolvedValue({
      items: [
        {
          id: 11,
          name: 'ops-key',
          key: 'sk-admin-test-key-1234567890',
          status: 'active',
          concurrency: 0,
          group_id: null,
          created_at: '2026-09-16T00:00:00Z',
        },
      ],
    })
    getAllGroups.mockResolvedValue([])
    updateApiKey.mockResolvedValue({
      api_key: {
        id: 11,
        name: 'ops-key',
        key: 'sk-admin-test-key-1234567890',
        status: 'active',
        concurrency: 2,
        group_id: null,
        created_at: '2026-09-16T00:00:00Z',
      },
    })
  })

  it('lets admins edit per-key concurrency', async () => {
    const wrapper = mount(UserApiKeysModal, {
      props: {
        show: false,
        user: { id: 7, email: 'admin@example.com', username: 'admin' },
      },
      global: {
        stubs: {
          BaseDialog: {
            props: ['show'],
            template: '<div v-if="show"><slot /></div>',
          },
          GroupBadge: true,
          GroupOptionItem: true,
          Teleport: true,
        },
      },
    })
    await wrapper.setProps({ show: true })
    await flushPromises()
    const input = wrapper.get('[data-testid="admin-key-concurrency"]')
    expect((input.element as HTMLInputElement).value).toBe('0')
    await input.setValue(2)
    await input.trigger('change')
    await flushPromises()
    expect(updateApiKey).toHaveBeenCalledWith(11, { concurrency: 2 })
  })
})
