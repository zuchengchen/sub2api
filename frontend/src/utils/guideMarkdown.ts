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

  parsed.body.querySelectorAll<HTMLAnchorElement>('a[href]').forEach((anchor) => {
    const href = anchor.getAttribute('href') || ''
    if (/^https?:\/\//i.test(href)) {
      anchor.target = '_blank'
      anchor.rel = 'noopener noreferrer'
    }
  })

  return {
    html: parsed.body.innerHTML,
    sections,
  }
}
