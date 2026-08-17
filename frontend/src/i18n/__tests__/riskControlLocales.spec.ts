import { describe, expect, it } from 'vitest'

import en from '../locales/en'
import zh from '../locales/zh'

describe('risk control locale copy', () => {
  it('uses one Risk Control name and matching settings copy', () => {
    expect(zh.nav.riskControl).toBe('风控中心')
    expect(en.nav.riskControl).toBe('Risk Control')
    expect(zh.admin.riskControl.saveConfig).toBe('保存风控配置')
    expect(en.admin.riskControl.saveConfig).toBe('Save Risk Settings')
  })

  it('describes ordered failover and independent Shadow stages', () => {
    expect(zh.admin.riskControl.deepseekChannelsSummary).toContain('按优先级顺序')
    expect(zh.admin.riskControl.layerStagesSummary).toContain('Shadow')
    expect(en.admin.riskControl.deepseekChannelsSummary).toContain('priority order')
    expect(en.admin.riskControl.layerStagesSummary).toContain('Shadow')
  })

  it('labels API keys as encrypted and never echoed', () => {
    expect(zh.admin.riskControl.channelKeyStored).toContain('已加密保存')
    expect(zh.admin.riskControl.channelKeyWillReplace).toContain('不会回显明文')
    expect(en.admin.riskControl.channelKeyStored).toContain('Encrypted')
    expect(en.admin.riskControl.channelKeyWillReplace).toContain('never echoed')
  })

  it('covers every expanded risk category in both locales', () => {
    const expected = ['cyber', 'accountAbuse', 'deepfakeDoxThreat', 'selfHarm', 'weapons', 'sexualContent']
    expect(Object.keys(zh.admin.riskControl.policyCategories)).toEqual(expected)
    expect(Object.keys(en.admin.riskControl.policyCategories)).toEqual(expected)
  })

  it('labels every production Layer 2 dedup cache counter', () => {
    expect(zh.admin.riskControl.overview.secondLayerCache).toContain('去重缓存')
    expect(en.admin.riskControl.overview.secondLayerCache).toContain('Dedup Cache')
    expect(zh.admin.riskControl.overview.cacheHits).toContain('{count}')
    expect(en.admin.riskControl.overview.cacheHits).toContain('{count}')
    for (const token of ['{misses}', '{writes}', '{errors}']) {
      expect(zh.admin.riskControl.overview.cacheActivity).toContain(token)
      expect(en.admin.riskControl.overview.cacheActivity).toContain(token)
    }
  })
})
