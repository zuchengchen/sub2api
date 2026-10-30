import { onScopeDispose, ref, watch } from 'vue'
import { adminAPI } from '@/api/admin'
import type { WindowStats } from '@/types'

/** One view-local snapshot: never share validators across pages, filters or days. */
export function useAccountTodayStats(options: {
  accountIDs: () => number[]
  context: () => unknown
}) {
  const stats = ref<Record<string, WindowStats>>({})
  const loading = ref(false)
  const error = ref<string | null>(null)
  let snapshotKey = ''
  let etag: string | null = null
  let requestVersion = 0
  let controller: AbortController | null = null

  const ids = () => [...new Set(options.accountIDs())].sort((a, b) => a - b)
  const contextKey = () => JSON.stringify([ids(), options.context()])
  // The server owns its reporting timezone and validator lifetime. Also discard
  // our validator on local/UTC midnight; every refresh still revalidates remotely.
  const key = () => {
    const now = new Date()
    return JSON.stringify([contextKey(), now.toDateString(), now.toISOString().slice(0, 10)])
  }
  const cancel = () => {
    requestVersion++
    controller?.abort()
    controller = null
    loading.value = false
  }
  const clear = () => {
    cancel()
    snapshotKey = ''
    etag = null
    stats.value = {}
    error.value = null
  }
  // Invalidate immediately, even while the new list request is still pending.
  watch(contextKey, clear, { flush: 'sync' })
  onScopeDispose(cancel)

  const refresh = async () => {
    const accountIDs = ids()
    const requestKey = key()
    if (accountIDs.length === 0) {
      clear()
      return
    }
    if (snapshotKey !== requestKey) clear()
    else cancel()
    const version = requestVersion
    controller = new AbortController()
    loading.value = true
    error.value = null
    try {
      const result = await adminAPI.accounts.getBatchTodayStatsWithEtag(accountIDs, {
        etag,
        signal: controller.signal
      })
      if (version !== requestVersion || requestKey !== key()) return
      if (result.notModified) {
        // A 304 has no body; retain the same reactive object and its rows.
        if (result.etag) etag = result.etag
        return
      }
      const serverStats = result.data?.stats ?? {}
      stats.value = Object.fromEntries(accountIDs.map(id => [String(id), serverStats[String(id)] ?? {
        requests: 0, tokens: 0, cost: 0, standard_cost: 0, user_cost: 0
      }]))
      snapshotKey = requestKey
      etag = result.etag
    } catch (cause) {
      if (version !== requestVersion || requestKey !== key()) return
      error.value = 'Failed'
      console.error('Failed to load account today stats:', cause)
    } finally {
      if (version === requestVersion) {
        loading.value = false
        controller = null
      }
    }
  }

  return { stats, loading, error, refresh }
}
