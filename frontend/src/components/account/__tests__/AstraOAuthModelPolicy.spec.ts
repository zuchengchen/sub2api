import { describe, expect, it, vi } from 'vitest'
vi.mock('vue-i18n', async () => {
  const { createRequire } = await import('node:module')
  return vi.importActual(createRequire(import.meta.url).resolve('vue-i18n/dist/vue-i18n.esm-bundler.js'))
})
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import AstraOAuthModelPolicy from '../AstraOAuthModelPolicy.vue'
import TiboRouteSpark from '../TiboRouteSpark.vue'
import AccountStatusIndicator from '../AccountStatusIndicator.vue'
import { buildOpenAIAstraPolicyRefreshKey, buildOpenAIUsageRefreshKey } from '@/utils/accountUsageRefresh'
import zh from '@/i18n/locales/zh/admin/accounts'
import en from '@/i18n/locales/en/admin/accounts'
import type { Account } from '@/types'

const at = '2026-11-02T10:00:00Z'
const policy = (verdict: 'healthy' | 'degraded' | 'unknown'): NonNullable<Account['openai_oauth_model_policy']> => ({
  verdict, scope: verdict === 'healthy' ? 'non_luna' : 'luna_and_images', sampled_at: verdict === 'unknown' ? undefined : at
})
const global = (locale = 'zh') => ({
  plugins: [createI18n({ legacy: false, locale, messages: { zh: { admin: zh }, en: { admin: en } } })],
  stubs: { Icon: true }
})

describe('Astra OAuth automatic model policy', () => {
  it.each(['zh', 'en'])('renders all states and live changes in %s without an edit action', async locale => {
    const wrapper = mount(AstraOAuthModelPolicy, { props: { policy: policy('healthy') }, global: global(locale) })
    const copy = locale === 'zh' ? zh.accounts.openai : en.accounts.openai
    for (const verdict of ['healthy', 'degraded', 'unknown', 'healthy'] as const) {
      await wrapper.setProps({ policy: policy(verdict) })
      expect(wrapper.attributes('data-verdict')).toBe(verdict)
      expect(wrapper.text()).toContain(copy.astraPolicyRange[verdict])
      expect(wrapper.text()).toContain(copy.astraPolicyDescription)
      if (verdict !== 'unknown') expect(wrapper.text()).toContain(at)
      expect(wrapper.find('input, select, button').exists()).toBe(false)
    }
    wrapper.unmount()
  })

  it('uses the initial restriction when there is no sample, including a newly created account', () => {
    const wrapper = mount(AstraOAuthModelPolicy, { global: global() })
    expect(wrapper.text()).toContain(zh.accounts.openai.astraPolicyRange.unknown)
    wrapper.unmount()
  })

  it('refreshes policy and dots even when updated_at and usage have not changed', () => {
    const account = { id: 8, platform: 'openai', type: 'oauth', updated_at: at,
      credentials: { model_mapping: { 'gpt-6-luna': 'gpt-6-luna' } },
      openai_oauth_model_policy: policy('healthy') } as Account
    const before = buildOpenAIAstraPolicyRefreshKey(account)
    const after = buildOpenAIAstraPolicyRefreshKey({ ...account, openai_oauth_model_policy: policy('degraded') })
    expect(after).not.toBe(before)
    expect(buildOpenAIUsageRefreshKey({ ...account, openai_oauth_model_policy: policy('degraded') } as Account)).toBe(buildOpenAIUsageRefreshKey(account))
    const history = buildOpenAIAstraPolicyRefreshKey({ ...account, codex_turn_tickets: [{ model: 'gpt-6-astra', tibo_routes: [{
      route: 'http', verdict: 'degraded', probes_hour: 1, flips_hour: 0, recent_samples: [{ verdict: 'healthy', at }]
    }] }] })
    expect(history).not.toBe(before)
  })

  it('preserves 24 dots and a manual pause while the policy changes', async () => {
    const account = { id: 8, platform: 'openai', type: 'oauth', status: 'active', schedulable: false,
      extra: {}, openai_oauth_model_policy: policy('degraded') } as Account
    const status = mount(AccountStatusIndicator, { props: { account }, global: global() })
    const history = mount(TiboRouteSpark, { props: { model: 'gpt-6-astra', verdict: 'degraded', samples: [{ verdict: 'healthy', at }] }, global: global() })
    const display = mount(AstraOAuthModelPolicy, { props: { policy: account.openai_oauth_model_policy }, global: global() })
    await status.setProps({ account: { ...account, openai_oauth_model_policy: policy('healthy') } })
    await display.setProps({ policy: policy('healthy') })
    expect(status.text()).toContain(zh.accounts.status.paused)
    expect(history.findAll('[data-testid="tibo-dot"]')).toHaveLength(24)
    expect(history.findAll('[data-testid="tibo-dot"]')[23].classes()).toContain('bg-emerald-500')
    expect(display.text()).toContain(zh.accounts.openai.astraPolicyRange.healthy)
    status.unmount(); history.unmount(); display.unmount()
  })
})
