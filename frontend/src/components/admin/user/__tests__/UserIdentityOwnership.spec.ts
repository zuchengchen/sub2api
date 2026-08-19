import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { AdminUser } from '@/types'

const { createUser, updateUser, showError, showSuccess } = vi.hoisted(() => ({
  createUser: vi.fn(),
  updateUser: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    users: {
      create: createUser,
      update: updateUser
    },
    userAttributes: {
      updateUserAttributeValues: vi.fn()
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess })
}))

vi.mock('@/composables/useStepUp', () => ({
  useStepUp: () => ({ run: (operation: () => unknown) => operation() }),
  isStepUpBlocked: () => false,
  isStepUpCancelled: () => false,
  stepUpBlockReason: () => ''
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copyToClipboard: vi.fn() })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

import UserCreateModal from '../UserCreateModal.vue'
import UserEditModal from '../UserEditModal.vue'

const global = {
  stubs: {
    BaseDialog: {
      props: ['show'],
      template: '<div v-if="show"><slot /><slot name="footer" /></div>'
    },
    Icon: true,
    TotpStepUpDialog: true,
    UserAttributeForm: true
  }
}

const user: AdminUser = {
  id: 17,
  email: 'member@example.com',
  username: 'member-owned-name',
  notes: 'old admin note',
  role: 'user',
  balance: 0,
  concurrency: 2,
  rpm_limit: 0,
  status: 'active',
  allowed_groups: [],
  balance_notify_enabled: false,
  balance_notify_threshold: null,
  balance_notify_extra_emails: [],
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z'
}

describe('admin user identity ownership', () => {
  beforeEach(() => {
    createUser.mockReset().mockResolvedValue({})
    updateUser.mockReset().mockResolvedValue({})
    showError.mockReset()
    showSuccess.mockReset()
  })

  it('stores the create dialog value as notes and omits username', async () => {
    const wrapper = mount(UserCreateModal, { props: { show: true }, global })

    await wrapper.find('input[type="email"]').setValue('new@example.com')
    await wrapper.find('input[type="text"]').setValue('password-value')
    await wrapper.get('[data-test="admin-notes"]').setValue('operator-only label')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(createUser).toHaveBeenCalledWith(expect.objectContaining({
      email: 'new@example.com',
      password: 'password-value',
      notes: 'operator-only label'
    }))
    expect(createUser.mock.calls[0][0]).not.toHaveProperty('username')
  })

  it('keeps the user username read-only and submits only the admin note', async () => {
    const wrapper = mount(UserEditModal, { props: { show: true, user }, global })

    expect(wrapper.get('[data-test="username-display"]').text()).toBe('member-owned-name')
    await wrapper.get('[data-test="admin-notes"]').setValue('updated operator note')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(updateUser).toHaveBeenCalledWith(17, expect.objectContaining({
      email: 'member@example.com',
      notes: 'updated operator note'
    }))
    expect(updateUser.mock.calls[0][1]).not.toHaveProperty('username')
  })
})
