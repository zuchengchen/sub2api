import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountActionMenu from '../AccountActionMenu.vue'
import type { Account } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'test-account',
    platform: 'openai',
    type: 'oauth',
    proxy_id: null,
    concurrency: 3,
    priority: 50,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: false,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides,
  }
}

const anchorRect = new DOMRect(100, 100, 24, 24)
const getBodyText = () => document.body.textContent ?? ''

describe('AccountActionMenu actions', () => {
  it.each(['apikey', 'oauth'] as const)('keeps only standard actions and a clickable manual test for %s accounts', async (type) => {
    const account = makeAccount({ type })
    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account, anchorRect },
      attachTo: document.body,
    })
    const buttons = Array.from(document.body.querySelectorAll('.action-menu-content button'))
    expect(buttons.map(button => button.textContent?.trim())).toEqual([
      'admin.accounts.testConnection',
      'admin.accounts.viewStats',
      ...(type === 'apikey'
        ? ['admin.accounts.duplicateAccount']
        : ['admin.accounts.reAuthorize', 'admin.accounts.refreshToken', 'admin.accounts.setPrivacy']),
    ])

    const testButton = buttons.find(button => button.textContent?.trim() === 'admin.accounts.testConnection') as HTMLButtonElement
    expect(testButton.disabled).toBe(false)
    testButton.click()
    await wrapper.vm.$nextTick()

    expect(wrapper.emitted('test')).toEqual([[account]])
    expect(wrapper.emitted('close')).toHaveLength(1)
    wrapper.unmount()
  })

  it('shows duplicate for API key accounts', () => {
    const account = makeAccount({ platform: 'anthropic', type: 'apikey' })
    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account, anchorRect },
      attachTo: document.body,
    })
    expect(getBodyText()).toContain('admin.accounts.duplicateAccount')
    wrapper.unmount()
  })

  it.each(['oauth', 'setup-token'] as const)('hides duplicate for %s accounts with rotating tokens', (type) => {
    const account = makeAccount({ type })
    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account, anchorRect },
      attachTo: document.body,
    })
    expect(getBodyText()).not.toContain('admin.accounts.duplicateAccount')
    wrapper.unmount()
  })

  it('emits duplicate with the selected account and closes the menu', async () => {
    const account = makeAccount({ platform: 'anthropic', type: 'apikey' })
    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account, anchorRect },
      attachTo: document.body,
    })

    const button = Array.from(document.body.querySelectorAll('button'))
      .find(button => button.textContent?.includes('admin.accounts.duplicateAccount'))
    expect(button).toBeDefined()
    button!.click()
    await wrapper.vm.$nextTick()

    expect(wrapper.emitted('duplicate')).toEqual([[account]])
    expect(wrapper.emitted('close')).toHaveLength(1)
    wrapper.unmount()
  })

  it('shows credential and privacy actions for OpenAI OAuth accounts', () => {
    const account = makeAccount({})
    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account, anchorRect },
      attachTo: document.body,
    })
    const body = getBodyText()
    expect(body).toContain('admin.accounts.reAuthorize')
    expect(body).toContain('admin.accounts.refreshToken')
    expect(body).toContain('admin.accounts.setPrivacy')
    wrapper.unmount()
  })
})
