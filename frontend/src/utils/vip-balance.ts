import type { User } from '@/types'

/**
 * VIP 冻结准备金（与后端 service.VipFrozenReserve 同源）。
 * VIP 的冻结是逻辑冻结：计费资格按「总余额 - 准备金」判定，
 * 不写入 users.frozen_balance（该字段属于批量图片余额暂扣台账）。
 */
export const VIP_FROZEN_RESERVE = 100

/** 展示用冻结金额：VIP = 准备金 + 在途暂扣；普通用户 = 在途暂扣。 */
export function displayFrozenBalance(user: Pick<User, 'balance' | 'frozen_balance' | 'is_vip'> | null | undefined): number {
  const holds = Number(user?.frozen_balance || 0)
  if (!user?.is_vip) {
    return holds
  }
  return VIP_FROZEN_RESERVE + holds
}

/** 展示用可用余额：VIP 扣除准备金，且不为负。 */
export function displayAvailableBalance(user: Pick<User, 'balance' | 'frozen_balance' | 'is_vip'> | null | undefined): number {
  const balance = Number(user?.balance || 0)
  if (!user?.is_vip) {
    return balance
  }
  return Math.max(balance - VIP_FROZEN_RESERVE, 0)
}
