import { describe, expect, it } from 'vitest'

import { displayAvailableBalance, displayFrozenBalance } from '../vip-balance'

describe('vip balance display helpers', () => {
  it('non-vip users keep raw ledger values', () => {
    const user = { balance: 50, frozen_balance: 5, is_vip: false }
    expect(displayFrozenBalance(user)).toBe(5)
    expect(displayAvailableBalance(user)).toBe(50)
  })

  it('vip users spend the full balance with no reserve', () => {
    const user = { balance: 139, frozen_balance: 0, is_vip: true }
    expect(displayFrozenBalance(user)).toBe(0)
    expect(displayAvailableBalance(user)).toBe(139)
  })

  it('vip frozen display is only the transient batch hold', () => {
    const user = { balance: 139, frozen_balance: 7, is_vip: true }
    expect(displayFrozenBalance(user)).toBe(7)
    expect(displayAvailableBalance(user)).toBe(139)
  })

  it('handles missing user fields', () => {
    expect(displayFrozenBalance(null)).toBe(0)
    expect(displayAvailableBalance({ balance: 10 })).toBe(10)
  })
})
