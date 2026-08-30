import { describe, expect, it } from 'vitest'
import { resolveApiEndpoint, resolveApiEndpointFromSettings } from '../apiEndpoint'

describe('resolveApiEndpoint', () => {
  it('保持配置地址不变（未开启跟随域名）', () => {
    expect(
      resolveApiEndpoint({
        configured: 'https://key66.vip',
        followHost: false,
        currentOrigin: 'https://key66.cc.cd',
      }),
    ).toBe('https://key66.vip')
  })

  it('开启跟随域名后切换到当前访问域名', () => {
    expect(
      resolveApiEndpoint({
        configured: 'https://key66.vip',
        followHost: true,
        currentOrigin: 'https://key66.cc.cd',
      }),
    ).toBe('https://key66.cc.cd')
  })

  it('跟随域名时保留配置地址的路径后缀', () => {
    expect(
      resolveApiEndpoint({
        configured: 'https://key66.vip/v1',
        followHost: true,
        currentOrigin: 'https://key66.cc.cd',
      }),
    ).toBe('https://key66.cc.cd/v1')
  })

  it('跟随域名时保留非默认端口', () => {
    expect(
      resolveApiEndpoint({
        configured: 'https://key66.vip/v1',
        followHost: true,
        currentOrigin: 'http://localhost:5173',
      }),
    ).toBe('http://localhost:5173/v1')
  })

  it('未配置地址时跟随域名只用当前来源', () => {
    expect(
      resolveApiEndpoint({
        configured: '',
        followHost: true,
        currentOrigin: 'https://key66.cc.cd',
      }),
    ).toBe('https://key66.cc.cd')
  })

  it('未配置地址且未开启跟随时回退到当前来源', () => {
    expect(
      resolveApiEndpoint({
        configured: '',
        followHost: false,
        currentOrigin: 'https://key66.cc.cd',
      }),
    ).toBe('https://key66.cc.cd')
  })

  it('去掉尾部斜杠避免出现双斜杠', () => {
    expect(
      resolveApiEndpoint({
        configured: 'https://key66.vip/v1/',
        followHost: true,
        currentOrigin: 'https://key66.cc.cd/',
      }),
    ).toBe('https://key66.cc.cd/v1')
  })

  it('无当前来源时（SSR）退回配置地址', () => {
    expect(
      resolveApiEndpoint({
        configured: 'https://key66.vip/v1',
        followHost: true,
        currentOrigin: '',
      }),
    ).toBe('https://key66.vip/v1')
  })

  it('配置地址不可解析时跟随域名仍返回当前来源', () => {
    expect(
      resolveApiEndpoint({
        configured: 'key66.vip/v1',
        followHost: true,
        currentOrigin: 'https://key66.cc.cd',
      }),
    ).toBe('https://key66.cc.cd')
  })

  it('两者都为空时返回空字符串', () => {
    expect(
      resolveApiEndpoint({ configured: '', followHost: true, currentOrigin: '' }),
    ).toBe('')
  })
})

describe('resolveApiEndpointFromSettings', () => {
  it('读取 public settings 的开关字段', () => {
    expect(
      resolveApiEndpointFromSettings(
        { api_base_url: 'https://key66.vip/v1', api_base_url_follow_host: true },
        'https://mofa.love.gd',
      ),
    ).toBe('https://mofa.love.gd/v1')
  })

  it('开关缺失（旧后端）时保持配置地址', () => {
    expect(
      resolveApiEndpointFromSettings(
        { api_base_url: 'https://key66.vip/v1' },
        'https://mofa.love.gd',
      ),
    ).toBe('https://key66.vip/v1')
  })

  it('settings 为空时返回当前来源', () => {
    expect(resolveApiEndpointFromSettings(null, 'https://mofa.love.gd')).toBe(
      'https://mofa.love.gd',
    )
  })
})
