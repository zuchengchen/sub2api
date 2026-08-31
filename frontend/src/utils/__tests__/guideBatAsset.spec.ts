import { createHash } from 'node:crypto'
import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { validateGuideBat } from '@/utils/guideBatValidation'

const dir = dirname(fileURLToPath(import.meta.url))
const batPath = resolve(dir, '../../../public/downloads/select-fastest-codex-base-url.bat')
const checksumPath = `${batPath}.sha256`
const guideDir = resolve(dir, '../../../../docs/guide')
const guideViewPath = resolve(dir, '../../views/public/GuideView.vue')
const vipPath = resolve(dir, '../../../../backend/internal/service/vip.go')
const batBytes = readFileSync(batPath)
const bat = batBytes.toString('ascii')
const checksum = readFileSync(checksumPath, 'utf8').trim()

// The guide is one file per chapter; join them in filename order so the
// assertions below still inspect the whole published document.
const guideChapterFiles = readdirSync(guideDir)
  .filter((name) => /^\d{3}-[a-z0-9-]+\.md$/.test(name))
  .sort()
const guide = guideChapterFiles
  .map((name) => readFileSync(resolve(guideDir, name), 'utf8').replace(/\s+$/, ''))
  .join('\n\n')
const guideViewSource = readFileSync(guideViewPath, 'utf8')
const vipSource = readFileSync(vipPath, 'utf8')

describe('guide BAT asset', () => {
  it('passes the calibrated structural safety validation', () => {
    expect(validateGuideBat(bat)).toEqual([])

    const benign = `${bat}\n# model_provider = "comment only"\n# api_key = "comment only"\n# Set-TomlSetting is historical text\n$monkey = 'similar text'\n`
    expect(validateGuideBat(benign)).toEqual([])

    expect(validateGuideBat(bat.replace("    'https://key66.vip',\n", '')))
      .toEqual(expect.arrayContaining([expect.objectContaining({ code: 'domain-list' })]))
    expect(validateGuideBat(`${bat}\nmodel_provider = "other"\n`))
      .toEqual(expect.arrayContaining([expect.objectContaining({ code: 'model-provider-write' })]))
    expect(validateGuideBat(`${bat}\napi_key = "not-a-real-key"\n`))
      .toEqual(expect.arrayContaining([expect.objectContaining({ code: 'credential-reference' })]))
    expect(validateGuideBat(bat.replace('[IO.File]::Replace', '[IO.File]::Move')))
      .toEqual(expect.arrayContaining([expect.objectContaining({ code: 'atomic-replace' })]))
  })

  it('uses the exact approved hosts and only a scoped base_url update path', () => {
    expect(bat).toContain("$hostIp = '15.204.82.11'")
    expect(bat).toContain("$MeasuredAttempts = 3")
    expect(bat).toContain("+ '/health'")
    expect(bat).toContain('function Get-CodexConfigTarget')
    expect(bat).toContain('function Set-UrlHostOnly')
    expect(bat).not.toMatch(/^\s*model_provider\s*=/im)
    expect(bat).not.toMatch(/Set-TomlSetting/i)
    expect(bat).not.toMatch(/\bapi[_ -]?key\b/i)
  })

  it('has a byte-accurate sidecar and the same digest in the guide', () => {
    const digest = createHash('sha256').update(batBytes).digest('hex')
    expect(checksum).toBe(`${digest}  select-fastest-codex-base-url.bat`)
    expect(guide.match(new RegExp(digest, 'g'))).toHaveLength(2)
    expect([...bat].every((character) => character.charCodeAt(0) <= 0x7f)).toBe(true)
  })

  it('keeps the bundled guide version in sync between the document and the view', () => {
    const documentVersion = guide.match(/^- 教程版本：`([^`]+)`$/m)?.[1]
    const viewVersion = guideViewSource.match(/^const bundledGuideVersion = '([^']+)'$/m)?.[1]

    // Both patterns must actually match; an absent version is a failure, not a passing match.
    expect(documentVersion).toBe('1.3')
    expect(viewVersion).toBe('1.3')
    expect(documentVersion).toBe(viewVersion)
  })

  it('ships the guide as one file per chapter, each starting with its heading', () => {
    expect(guideChapterFiles).toHaveLength(16)

    for (const name of guideChapterFiles) {
      const content = readFileSync(resolve(guideDir, name), 'utf8')
      expect(content.startsWith('## '), `${name} must start with a ## heading`).toBe(true)
    }

    // The single-file guide must be gone so no build path can read a stale copy.
    expect(existsSync(resolve(guideDir, '../guide.zh.md'))).toBe(false)
  })

  it('keeps the published SVIP rules synchronized with backend constants', () => {
    const constants = {
      threshold: vipSource.match(/VipBalanceThreshold\s*=\s*([\d.]+)/)?.[1],
      reserve: vipSource.match(/VipFrozenReserve\s*=\s*([\d.]+)/)?.[1],
      discount: vipSource.match(/VipRateDiscount\s*=\s*([\d.]+)/)?.[1],
      group: vipSource.match(/VipDiscountedGroupName\s*=\s*"([^"]+)"/)?.[1],
      model: vipSource.match(/VipExclusiveModelName\s*=\s*"([^"]+)"/)?.[1],
    }

    expect(constants).toEqual({
      threshold: '100.0',
      reserve: '100.0',
      discount: '0.05',
      group: 'gpt-pro',
      model: 'gpt-5.6-luna',
    })
    expect(guide).toContain('严格大于 100 元')
    expect(guide).toContain('冻结 100 元')
    expect(guide).toContain('倍率减 0.05')
    expect(guide).toContain('`gpt-pro`')
    expect(guide).toContain('`gpt-5.6-luna`')
  })
})
