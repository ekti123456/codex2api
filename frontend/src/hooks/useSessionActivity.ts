import { useEffect, useState } from 'react'
import { api } from '../api'
import type { SessionActivityPage } from '../lib/sessionErrors'

// A separate, current-page-only poller. No list/count query runs on this timer.
export function useSessionActivity(keys: string[], enabled: boolean, queryKey: string) {
  const keyList = JSON.stringify(keys.slice(0, 100))
  const subscription = `${queryKey}:${keyList}`
  const [snapshot, setSnapshot] = useState<{ subscription: string; page: SessionActivityPage } | null>(null)
  useEffect(() => {
    setSnapshot(null)
    const requestedKeys = JSON.parse(keyList) as string[]
    if (!enabled || requestedKeys.length === 0) return
    let disposed = false
    let generation = 0
    let controller: AbortController | null = null
    let timer: ReturnType<typeof setTimeout> | undefined
    const load = async () => {
      if (disposed || document.visibilityState !== 'visible') return
      const current = ++generation
      controller?.abort()
      controller = new AbortController()
      const requestController = controller
      const timeout = setTimeout(() => requestController.abort(), 10000)
      try {
        const page = await api.getSessionActivities(requestedKeys, controller.signal)
        if (!disposed && current === generation) setSnapshot({ subscription, page })
      } catch {
        // Never leave an old green "running" badge on screen after a failed poll.
        if (!disposed && current === generation) setSnapshot(null)
      } finally {
        clearTimeout(timeout)
        if (!disposed && current === generation && document.visibilityState === 'visible') timer = setTimeout(() => void load(), 15000)
      }
    }
    const visibilityChanged = () => {
      clearTimeout(timer)
      generation++
      controller?.abort()
      setSnapshot(null)
      if (document.visibilityState === 'visible') void load()
    }
    document.addEventListener('visibilitychange', visibilityChanged)
    void load()
    return () => {
      disposed = true
      generation++
      clearTimeout(timer)
      controller?.abort()
      document.removeEventListener('visibilitychange', visibilityChanged)
    }
  }, [keyList, enabled, subscription])
  return enabled && snapshot?.subscription === subscription ? snapshot.page : null
}
