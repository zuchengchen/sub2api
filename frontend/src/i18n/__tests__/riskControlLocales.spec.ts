import { describe, expect, it } from 'vitest'

import en from '../locales/en'
import zh from '../locales/zh'

describe('risk control locale copy', () => {
  it('keeps synchronous counters separate from async record processing', () => {
    expect(zh.admin.riskControl.preBlockSyncHint).toContain('不包含异步写记录任务')
    expect(en.admin.riskControl.preBlockSyncHint).toContain('excluding async record tasks')
  })

  it('summarizes only fields exposed by the synchronous audit-key status', () => {
    expect(zh.admin.riskControl.preBlockAPIKeyLoadSummary).toBe('同步并发 {active} / 可用 Key {available}，累计 {total} 次')
    expect(en.admin.riskControl.preBlockAPIKeyLoadSummary).toBe('Sync active {active} / usable keys {available}, {total} total')
  })

  it('does not describe pre-block audit key polling as bypassing the worker pool', () => {
    expect(zh.admin.riskControl.preBlockAPIKeyLoadHint).toBe('同步前置拦截直接轮询可用审核 Key。')
    expect(zh.admin.riskControl.preBlockAPIKeyLoadHint).not.toContain('Worker 池')
    expect(en.admin.riskControl.preBlockAPIKeyLoadHint).not.toContain('worker pool')
  })

  it('labels stored windows as the evidence actually sent for moderation', () => {
    expect(zh.admin.riskControl.inputDetailContent).toBe('实际送审证据')
    expect(zh.admin.riskControl.evidenceMatches).toBe('全部命中词')
    expect(en.admin.riskControl.inputDetailContent).toBe('Evidence Sent for Review')
    expect(en.admin.riskControl.evidenceMatches).toBe('All matched keywords')
  })
})
