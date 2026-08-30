import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import GuideView from '@/views/public/GuideView.vue'
import { buildGuideDocument, extractGuideCommands } from '@/utils/guideMarkdown'
import guideMarkdown from '../../../../../docs/guide.zh.md?raw'

const copyToClipboard = vi.hoisted(() => vi.fn().mockResolvedValue(true))
const getPublicGuide = vi.hoisted(() => vi.fn())

vi.mock('@/api/guide', () => ({ getPublicGuide }))

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
    getPublicGuide.mockReset()
    getPublicGuide.mockResolvedValue({
      content: '',
      version: 0,
      updated_at: '',
      has_custom_content: false,
    })
    window.history.replaceState({}, '', '/guide')
    window.__APP_CONFIG__ = {
      site_name: '测试站点',
      site_logo: '',
      doc_url: '',
    } as typeof window.__APP_CONFIG__
  })

  it('replaces the bundled guide with a published database version', async () => {
    getPublicGuide.mockResolvedValue({
      content: '## 管理员发布的教程\n\n这是数据库中的内容。',
      version: 7,
      updated_at: '2026-08-30T12:00:00Z',
      has_custom_content: true,
    })

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
    await flushPromises()

    expect(wrapper.get('[data-test="guide-content"]').text()).toContain('这是数据库中的内容。')
    expect(wrapper.text()).toContain('教程版本 在线第 7 版')
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

    expect(wrapper.get('[data-test="guide-view"]').text()).toContain('从充值到第一次使用，照着做就可以')
    expect(wrapper.get('#recharge').text()).toBe('充值：先买兑换码，再兑换余额')
    expect(wrapper.get('#goal-workflow').text()).toBe('使用 goal-workflow 小助手')
    expect(wrapper.get('#svip').text()).toBe('SVIP 能得到什么、需要注意什么')
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
    expect(wrapper.get('[aria-live="polite"]').text()).toBe('安装 goal-workflow 已复制')
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
    expect(guideMarkdown).toContain('只会更换当前 Codex 配置中的网址域名')
    expect(guideMarkdown).toContain('可以把它理解为“软件专用密码”')
    expect(guideMarkdown).toContain('可以把 hosts 理解成电脑自己的“网址通讯录”')
    expect(guideMarkdown).toContain('SHA-256（可以理解为文件的“指纹”）')
    expect(guideMarkdown).not.toMatch(/A.{0,20}B.{0,80}(?:支付|订单)/s)
    expect(guideMarkdown).not.toContain('并发支付竞态')
    expect(guideMarkdown).not.toContain('没有支付但是到账')
  })
})
