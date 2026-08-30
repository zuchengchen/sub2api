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
  '快速开始': 'quick-start',
  '注册与登录': 'account',
  '充值：购买并兑换余额': 'recharge',
  '创建和保护 API Key': 'api-key',
  '完成第一次 API 调用': 'first-request',
  '三个可用域名': 'domains',
  '在 Codex 中使用本站': 'codex',
  '使用自动测速脚本': 'speed-script',
  '使用 goal-workflow skill': 'goal-workflow',
  'SVIP 权利与义务': 'svip',
  '查看余额、用量与请求记录': 'usage',
  '常见问题': 'faq',
  '安全建议': 'security',
  '获取帮助': 'support',
  '版本信息': 'version',
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
