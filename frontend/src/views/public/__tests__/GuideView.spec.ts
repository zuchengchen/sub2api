import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import GuideView from '@/views/public/GuideView.vue'
import { buildGuideDocument, extractGuideCommands } from '@/utils/guideMarkdown'
import guideMarkdown from '../../../../../docs/guide.zh.md?raw'

const copyToClipboard = vi.hoisted(() => vi.fn().mockResolvedValue(true))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ isAuthenticated: false, isAdmin: false }),
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copyToClipboard, copied: { value: false } }),
}))

describe('GuideView', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    copyToClipboard.mockClear()
    window.history.replaceState({}, '', '/guide')
    window.__APP_CONFIG__ = {
      site_name: '测试站点',
      site_logo: '',
      doc_url: '',
    } as typeof window.__APP_CONFIG__
  })

  it('renders the approved guide, stable table of contents, commands, and download', async () => {
    const wrapper = mount(GuideView, {
      global: {
        stubs: {
          RouterLink: {
            props: ['to'],
            template: '<a :href="to"><slot /></a>',
          },
        },
      },
    })

    expect(wrapper.get('[data-test="guide-view"]').text()).toContain('从充值到调用，一步完成配置')
    expect(wrapper.get('#recharge').text()).toBe('充值：购买并兑换余额')
    expect(wrapper.get('#goal-workflow').text()).toBe('使用 goal-workflow skill')
    expect(wrapper.get('#svip').text()).toBe('SVIP 权利与义务')
    expect(wrapper.get('[data-test="guide-download"]').attributes('href')).toBe(
      '/downloads/select-fastest-codex-base-url.bat'
    )

    const commands = extractGuideCommands(guideMarkdown)
    expect(commands.map((command) => command.id)).toEqual([
      'skill-install',
      'skill-update',
      'goal-example',
    ])

    await wrapper.get('[data-test="copy-skill-install"]').trigger('click')
    expect(copyToClipboard).toHaveBeenCalledWith(
      '安装 skill https://github.com/zuchengchen/goal-workflow/tree/master/skills/goal-workflow',
      '命令已复制'
    )
    expect(wrapper.get('[aria-live="polite"]').text()).toBe('安装 goal-workflow已复制')
  })

  it('sanitizes Markdown HTML and secures external links', () => {
    const document = buildGuideDocument(
      '## 标题\n\n<script>alert(1)</script><a href="javascript:alert(2)">危险</a>\n\n[外部](https://example.test/help)'
    )

    expect(document.html).not.toContain('<script')
    expect(document.html).not.toContain('javascript:')
    expect(document.html).toContain('target="_blank"')
    expect(document.html).toContain('rel="noopener noreferrer"')
    expect(document.sections).toEqual([{ id: 'section-1', label: '标题' }])
  })

  it('contains the requested workflows without disclosing the payment race', () => {
    expect(guideMarkdown).toContain('购买兑换码')
    expect(guideMarkdown).toContain('[兑换余额](/redeem)')
    expect(guideMarkdown).toContain('$goal-workflow 清理内存')
    expect(guideMarkdown).toContain('必须**严格大于 100 元**')
    expect(guideMarkdown).toContain('只替换当前 provider 的 `base_url` 主机名')
    expect(guideMarkdown).not.toMatch(/A.{0,20}B.{0,80}(?:支付|订单)/s)
    expect(guideMarkdown).not.toContain('并发支付竞态')
    expect(guideMarkdown).not.toContain('没有支付但是到账')
  })
})
