import { describe, expect, it } from 'vitest'

import { displayAvailableBalance, displayFrozenBalance } from '../vip-balance'

describe('vip balance display helpers', () => {
  it('non-vip users keep raw ledger values', () => {
    const user = { balance: 50, frozen_balance: 5, is_vip: false }
    expect(displayFrozenBalance(user)).toBe(5)
    expect(displayAvailableBalance(user)).toBe(50)
  })

  it('vip users show the 100 reserve as frozen and reduced available', () => {
    const user = { balance: 139, frozen_balance: 0, is_vip: true }
    expect(displayFrozenBalance(user)).toBe(100)
    expect(displayAvailableBalance(user)).toBe(39)
  })

  it('vip reserve adds on top of transient batch holds', () => {
    const user = { balance: 139, frozen_balance: 7, is_vip: true }
    expect(displayFrozenBalance(user)).toBe(107)
    expect(displayAvailableBalance(user)).toBe(39)
  })

  it('vip available never goes negative', () => {
    const user = { balance: 40, frozen_balance: 0, is_vip: true }
    expect(displayAvailableBalance(user)).toBe(0)
    expect(displayFrozenBalance(user)).toBe(100)
  })

  it('handles missing user fields', () => {
    expect(displayFrozenBalance(null)).toBe(0)
    expect(displayAvailableBalance({ balance: 10 })).toBe(10)
  })
})
