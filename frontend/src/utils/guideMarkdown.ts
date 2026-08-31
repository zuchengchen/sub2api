import DOMPurify from 'dompurify'
import { marked } from 'marked'

export interface GuideSection {
  id: string
  label: string
}

export interface GuideCommand {
  id: string
  content: string
}

export interface GuideDocument {
  html: string
  sections: GuideSection[]
}

/** One independently editable chapter. `slug` is the permanent anchor id. */
export interface GuideChapter {
  slug: string
  title: string
  content: string
}

export const guideSlugPattern = /^[a-z0-9]+(?:-[a-z0-9]+)*$/

const chapterSlugMaxLength = 48

/**
 * Legacy title-to-anchor map. Chapters now carry their own slug, but a guide
 * still published as a single Markdown document is split by these titles so
 * anchors such as #recharge survive the migration.
 */
const sectionIds: Record<string, string> = {
  '先照着做一遍': 'quick-start',
  '注册与登录': 'account',
  '充值：先买兑换码，再兑换余额': 'recharge',
  'API Key：给软件使用的专用密码': 'api-key',
  '检查 API Key 能不能用': 'first-request',
  '三个可用网址': 'domains',
  '在 Codex 中使用本站': 'codex',
  '自动选择速度最快的网址': 'speed-script',
  '使用 goal-workflow 小助手': 'goal-workflow',
  'SVIP 能得到什么、需要注意什么': 'svip',
  '查看余额和使用记录': 'usage',
  '错误码含义与处理方案': 'error-codes',
  '遇到问题先看这里': 'faq',
  '保护账户和密钥': 'security',
  '联系管理员时要准备什么': 'support',
  '教程版本': 'version',
}

export function extractGuideCommands(markdown: string): GuideCommand[] {
  const commands: GuideCommand[] = []
  const pattern = /<!--\s*copy-command:([a-z0-9-]+)\s*-->\s*```[^\r\n]*\r?\n([\s\S]*?)\r?\n```/g

  for (const match of markdown.matchAll(pattern)) {
    commands.push({
      id: match[1],
      content: match[2].trim(),
    })
  }

  return commands
}

/** Reads the `## ` heading that a chapter body must start with. */
export function readChapterTitle(content: string): string {
  const heading = /^##[ \t]+(.+?)[ \t]*$/m.exec(content)
  return heading ? heading[1].trim().replace(/\s+/g, ' ') : ''
}

/**
 * Derives a stable slug for a newly created chapter. ASCII words in the title
 * are preferred; a title without them (a Chinese-only heading, for example)
 * falls back to a deterministic digest so the slug is reproducible and unique.
 */
export function deriveChapterSlug(title: string, taken: Iterable<string> = []): string {
  const used = new Set(taken)
  const ascii = title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, chapterSlugMaxLength)
    .replace(/-+$/, '')

  // FNV-1a over the code points keeps the fallback deterministic across runs.
  let hash = 0x811c9dc5
  for (const character of title) {
    hash ^= character.codePointAt(0) || 0
    hash = Math.imul(hash, 0x01000193) >>> 0
  }
  const base = ascii || `section-${hash.toString(36)}`

  if (!used.has(base)) return base
  for (let suffix = 2; ; suffix += 1) {
    const candidate = `${base}-${suffix}`
    if (!used.has(candidate)) return candidate
  }
}

/**
 * Splits a single-document guide into chapters at every `##` heading. Content
 * before the first heading is preserved as a `preface` chapter rather than
 * dropped. Used to migrate a guide published before chapters existed.
 */
export function splitGuideIntoChapters(markdown: string): GuideChapter[] {
  const chapters: GuideChapter[] = []
  const used = new Set<string>()
  const preface: string[] = []
  let current: { title: string; lines: string[] } | null = null

  const push = (title: string, body: string) => {
    const content = body.replace(/\s+$/, '')
    if (!content.trim()) return
    const slug = sectionIds[title] && !used.has(sectionIds[title])
      ? sectionIds[title]
      : deriveChapterSlug(title, used)
    used.add(slug)
    chapters.push({ slug, title, content })
  }

  for (const line of markdown.split('\n')) {
    const heading = /^##[ \t]+(.+?)[ \t]*$/.exec(line)
    if (heading) {
      if (current) push(current.title, current.lines.join('\n'))
      current = { title: heading[1].trim().replace(/\s+/g, ' '), lines: [line] }
      continue
    }
    if (current) current.lines.push(line)
    else preface.push(line)
  }
  if (current) push(current.title, current.lines.join('\n'))

  if (preface.join('\n').trim()) {
    used.add('preface')
    chapters.unshift({
      slug: 'preface',
      title: readChapterTitle(preface.join('\n')) || '前言',
      content: preface.join('\n').replace(/\s+$/, ''),
    })
  }

  return chapters
}

/** Joins chapters back into the single Markdown document the renderer reads. */
export function joinGuideChapters(chapters: GuideChapter[]): string {
  return chapters
    .map((chapter) => chapter.content.replace(/\s+$/, ''))
    .filter((content) => content.trim() !== '')
    .join('\n\n')
}

/**
 * Renders chapters as one document, anchoring each chapter to its own slug so
 * links such as /guide#recharge stay valid however the chapter was edited.
 *
 * Each chapter is rendered separately and its anchor is attached to that
 * chapter's own fragment. Positional mapping would misalign as soon as one
 * chapter lacks a `##` heading — a `preface` chapter produced by splitting a
 * legacy guide, for example — silently shifting every later anchor by one.
 */
export function buildGuideDocumentFromChapters(chapters: GuideChapter[]): GuideDocument {
  const sections: GuideSection[] = []
  const usedIds = new Set<string>()
  const fragments: string[] = []

  for (const chapter of chapters) {
    if (chapter.content.trim() === '') continue

    const rendered = marked.parse(chapter.content, { gfm: true, breaks: false }) as string
    const parsed = new DOMParser().parseFromString(DOMPurify.sanitize(rendered), 'text/html')

    let id = chapter.slug || 'section'
    let suffix = 2
    while (usedIds.has(id)) {
      id = `${chapter.slug}-${suffix}`
      suffix += 1
    }
    usedIds.add(id)

    secureExternalLinks(parsed)

    const heading = parsed.body.querySelector('h2')
    if (heading) {
      heading.id = id
      sections.push({ id, label: (heading.textContent || '').trim().replace(/\s+/g, ' ') })
      fragments.push(parsed.body.innerHTML)
      continue
    }

    // No heading means no table-of-contents entry, but the anchor must still
    // resolve so an existing /guide#<slug> link does not break.
    const wrapper = parsed.createElement('section')
    wrapper.id = id
    wrapper.innerHTML = parsed.body.innerHTML
    fragments.push(wrapper.outerHTML)
  }

  return { html: fragments.join('\n'), sections }
}

function secureExternalLinks(parsed: Document): void {
  parsed.body.querySelectorAll<HTMLAnchorElement>('a[href]').forEach((anchor) => {
    const href = anchor.getAttribute('href') || ''
    if (/^https?:\/\//i.test(href)) {
      anchor.target = '_blank'
      anchor.rel = 'noopener noreferrer'
    }
  })
}

export function buildGuideDocument(markdown: string): GuideDocument {
  const rendered = marked.parse(markdown, { gfm: true, breaks: false }) as string
  const sanitized = DOMPurify.sanitize(rendered)
  const parsed = new DOMParser().parseFromString(sanitized, 'text/html')
  const sections: GuideSection[] = []
  const usedIds = new Set<string>()

  parsed.body.querySelectorAll('h2').forEach((heading, index) => {
    const label = (heading.textContent || '').trim().replace(/\s+/g, ' ')
    const preferredId = sectionIds[label] || `section-${index + 1}`
    let id = preferredId
    let suffix = 2

    while (usedIds.has(id)) {
      id = `${preferredId}-${suffix}`
      suffix += 1
    }

    usedIds.add(id)
    heading.id = id
    sections.push({ id, label })
  })

  secureExternalLinks(parsed)

  return {
    html: parsed.body.innerHTML,
    sections,
  }
}
