import type { AccountHealthBucket } from '../types'

export const ACCOUNT_HEALTH_BLOCK_COUNT = 20
export const ACCOUNT_HEALTH_BLOCK_MINUTES = 10

export function accountHealthOverloadSummary(
  buckets: AccountHealthBucket[] | undefined,
  blockCount = ACCOUNT_HEALTH_BLOCK_COUNT,
) {
  const visible = (buckets ?? []).slice(-blockCount)
  const count = visible.reduce((total, bucket) => {
    const value = bucket.overloaded_500
    return total + (typeof value === 'number' && Number.isSafeInteger(value) && value > 0 ? value : 0)
  }, 0)
  const startAt = Date.parse(visible[0]?.start_at ?? '')
  const endAt = Date.parse(visible[visible.length - 1]?.end_at ?? '')
  return {
    count,
    overloaded: count > 0,
    minutes: blockCount * ACCOUNT_HEALTH_BLOCK_MINUTES,
    startAt: Number.isFinite(startAt) && endAt > startAt ? startAt : undefined,
    endAt: Number.isFinite(endAt) && endAt > startAt ? endAt : undefined,
  }
}
