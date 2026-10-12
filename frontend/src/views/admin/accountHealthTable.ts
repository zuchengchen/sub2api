import type { AccountHealthSnapshot } from '@/types'

export type HealthSortKey = 'account' | 'platform' | 'score' | 'err_rate' | 'latency' | 'samples' | 'state'
export type HealthSortDir = 'asc' | 'desc'

const stateRank: Record<string, number> = { isolated: 0, degraded: 1, healthy: 2 }

export function defaultHealthSortDir(key: HealthSortKey): HealthSortDir {
  return key === 'err_rate' || key === 'latency' || key === 'samples' ? 'desc' : 'asc'
}

export function nextHealthSort(
  currentKey: HealthSortKey,
  currentDir: HealthSortDir,
  clicked: HealthSortKey
): { key: HealthSortKey; dir: HealthSortDir } {
  if (currentKey === clicked) {
    return { key: clicked, dir: currentDir === 'asc' ? 'desc' : 'asc' }
  }
  return { key: clicked, dir: defaultHealthSortDir(clicked) }
}

export function filterHealthItems(items: AccountHealthSnapshot[], platform: string): AccountHealthSnapshot[] {
  if (!platform) return items
  return items.filter(item => item.platform === platform)
}

function compareHealthItems(a: AccountHealthSnapshot, b: AccountHealthSnapshot, key: HealthSortKey): number {
  switch (key) {
    case 'account': {
      const names = (a.name || '').localeCompare(b.name || '', 'zh')
      return names !== 0 ? names : a.account_id - b.account_id
    }
    case 'platform':
      return a.platform.localeCompare(b.platform)
    case 'score':
      return a.score - b.score
    case 'err_rate':
      return a.err_rate - b.err_rate
    case 'latency':
      return (a.avg_latency_ms ?? Number.NEGATIVE_INFINITY) - (b.avg_latency_ms ?? Number.NEGATIVE_INFINITY)
    case 'samples':
      return a.total - b.total
    case 'state':
      return (stateRank[a.state] ?? 99) - (stateRank[b.state] ?? 99)
    default:
      return 0
  }
}

export function sortHealthItems(
  items: AccountHealthSnapshot[],
  key: HealthSortKey,
  dir: HealthSortDir
): AccountHealthSnapshot[] {
  const sign = dir === 'asc' ? 1 : -1
  return [...items].sort((a, b) => sign * compareHealthItems(a, b, key))
}

export function visibleHealthItems(
  items: AccountHealthSnapshot[],
  platform: string,
  key: HealthSortKey,
  dir: HealthSortDir
): AccountHealthSnapshot[] {
  return sortHealthItems(filterHealthItems(items, platform), key, dir)
}
