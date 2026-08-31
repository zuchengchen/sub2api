import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import GuideView from '@/views/public/GuideView.vue'
import {
  buildGuideDocument,
  buildGuideDocumentFromChapters,
  deriveChapterSlug,
  extractGuideCommands,
  joinGuideChapters,
  splitGuideIntoChapters,
} from '@/utils/guideMarkdown'
import { getBundledGuideChapters } from '@/utils/guideSections'

// The guide now ships as one file per chapter under docs/guide/; the joined
// document is what the page renders and what these assertions inspect.
const guideChapters = getBundledGuideChapters()
const guideMarkdown = joinGuideChapters(guideChapters)

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

  it('replaces the bundled guide with published database chapters', async () => {
    getPublicGuide.mockResolvedValue({
      content: '',
      chapters: [{
        slug: 'admin-chapter',
        title: '管理员发布的教程',
        content: '## 管理员发布的教程\n\n这是数据库中的内容。',
      }],
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
    // The published chapter slug becomes the anchor id.
    expect(wrapper.get('#admin-chapter').text()).toBe('管理员发布的教程')
  })

  it('splits a guide published before chapters existed and keeps its anchors', async () => {
    getPublicGuide.mockResolvedValue({
      content: '## 充值：先买兑换码，再兑换余额\n\n旧的整篇内容。\n\n## 遇到问题先看这里\n\n旧的 FAQ。',
      version: 4,
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

    expect(wrapper.get('#recharge').text()).toBe('充值：先买兑换码，再兑换余额')
    expect(wrapper.get('#faq').text()).toBe('遇到问题先看这里')
    expect(wrapper.get('[data-test="guide-content"]').text()).toContain('旧的整篇内容。')
  })

  it('ships one file per chapter with stable slugs and ordering', () => {
    expect(guideChapters).toHaveLength(16)
    expect(guideChapters.map((chapter) => chapter.slug)).toEqual([
      'quick-start',
      'account',
      'recharge',
      'api-key',
      'first-request',
      'domains',
      'codex',
      'speed-script',
      'goal-workflow',
      'svip',
      'usage',
      'error-codes',
      'faq',
      'security',
      'support',
      'version',
    ])

    // Every chapter must start with its own `##` heading and carry its title.
    for (const chapter of guideChapters) {
      expect(chapter.content.startsWith('## ')).toBe(true)
      expect(chapter.title).not.toBe('')
    }

    const { sections } = buildGuideDocumentFromChapters(guideChapters)
    expect(sections.map((section) => section.id)).toEqual(guideChapters.map((chapter) => chapter.slug))
  })

  it('anchors every chapter to its own slug even when one has no heading', () => {
    // Splitting a legacy guide yields a `preface` chapter with no `##` heading.
    // Mapping anchors by heading position shifted every later anchor by one, so
    // #recharge landed on the wrong section and the last slug vanished.
    const { html, sections } = buildGuideDocumentFromChapters([
      { slug: 'preface', title: '前言', content: '开头前言一段。' },
      { slug: 'recharge', title: '充值', content: '## 充值\n\n充值正文。' },
      { slug: 'faq', title: '常见问题', content: '## 常见问题\n\nFAQ 正文。' },
    ])

    // Headings map to their own chapter's slug, never a neighbour's.
    expect(html).toContain('<h2 id="recharge">充值</h2>')
    expect(html).toContain('<h2 id="faq">常见问题</h2>')
    // The headingless chapter still exposes a resolvable anchor.
    expect(html).toContain('id="preface"')
    expect(html).toContain('开头前言一段。')

    // Only chapters with headings appear in the table of contents.
    expect(sections).toEqual([
      { id: 'recharge', label: '充值' },
      { id: 'faq', label: '常见问题' },
    ])
  })

  it('keeps external links safe when rendering from chapters', () => {
    const { html } = buildGuideDocumentFromChapters([
      {
        slug: 'links',
        title: '链接',
        content: '## 链接\n\n<script>alert(1)</script>\n\n[外部](https://example.test/x)\n\n[危险](javascript:alert(2))',
      },
    ])

    expect(html).not.toContain('<script')
    expect(html).not.toContain('javascript:')
    expect(html).toContain('target="_blank"')
    expect(html).toContain('rel="noopener noreferrer"')
  })

  it('round-trips a single document through split and join', () => {
    const document = '前言段落。\n\n## 第一章\n\n正文一。\n\n## 第二章\n\n正文二。'
    const chapters = splitGuideIntoChapters(document)

    expect(chapters).toHaveLength(3)
    expect(chapters[0].slug).toBe('preface')
    expect(chapters[0].content).toContain('前言段落。')
    expect(chapters.map((chapter) => chapter.title)).toEqual(['前言', '第一章', '第二章'])
    // Chinese-only headings fall back to deterministic digest slugs.
    expect(chapters[1].slug).toMatch(/^section-[a-z0-9]+$/)
    expect(chapters[2].slug).not.toBe(chapters[1].slug)

    const rejoined = joinGuideChapters(chapters)
    for (const fragment of ['前言段落。', '正文一。', '正文二。']) {
      expect(rejoined).toContain(fragment)
    }
  })

  it('derives deterministic, collision-free slugs for new chapters', () => {
    expect(deriveChapterSlug('SVIP 说明')).toBe('svip')
    expect(deriveChapterSlug('Billing & Quota')).toBe('billing-quota')
    expect(deriveChapterSlug('SVIP 说明', ['svip'])).toBe('svip-2')

    // A title without ASCII words still yields a stable, repeatable slug.
    const chineseOnly = deriveChapterSlug('全新章节')
    expect(chineseOnly).toMatch(/^section-[a-z0-9]+$/)
    expect(deriveChapterSlug('全新章节')).toBe(chineseOnly)
    expect(deriveChapterSlug('另一章节')).not.toBe(chineseOnly)
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

  it('gives the error-code reference a stable anchor and covers all three code layers', () => {
    const { sections } = buildGuideDocument(guideMarkdown)

    expect(sections).toEqual(
      expect.arrayContaining([{ id: 'error-codes', label: '错误码含义与处理方案' }])
    )
    expect(sections.map((section) => section.id)).not.toContain('section-13')

    // HTTP status codes, business reason codes, and error.type names must all be explained.
    for (const code of ['`400`', '`401`', '`403`', '`404`', '`409`', '`413`', '`429`', '`499`', '`502`', '`503`', '`504`']) {
      expect(guideMarkdown).toContain(code)
    }
    for (const reason of [
      'INSUFFICIENT_BALANCE',
      'API_KEY_EXPIRED',
      'API_KEY_RATE_5H_EXCEEDED',
      'USER_RPM_EXCEEDED',
      'GROUP_RPM_EXCEEDED',
      'USER_PLATFORM_DAILY_QUOTA_EXHAUSTED',
      'SUBSCRIPTION_INVALID',
      'BILLING_SERVICE_ERROR',
    ]) {
      expect(guideMarkdown).toContain(reason)
    }
    for (const type of [
      'authentication_error',
      'permission_error',
      'billing_error',
      'invalid_request_error',
      'rate_limit_exceeded',
      'rate_limit_error',
      'upstream_error',
    ]) {
      expect(guideMarkdown).toContain(type)
    }
    expect(guideMarkdown).toContain('Retry-After')

    // The FAQ must defer to the reference instead of re-explaining status codes.
    expect(guideMarkdown).toContain('[错误码含义与处理方案](#error-codes)')
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
