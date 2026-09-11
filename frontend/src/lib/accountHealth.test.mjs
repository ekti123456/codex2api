import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { ACCOUNT_HEALTH_BLOCK_COUNT, ACCOUNT_HEALTH_BLOCK_MINUTES, accountHealthOverloadSummary } from './accountHealth.ts'

test('plan warning only uses the explicit 500 overload count, not other failures', () => {
  for (const buckets of [undefined, [], [{ success: 1, failed: 10 }], [{ success: 1, failed: 10, overloaded_500: 0 }]]) {
    const summary = accountHealthOverloadSummary(buckets)
    assert.equal(summary.count, 0)
    assert.equal(summary.overloaded, false)
  }
  const buckets = [{ success: 50, failed: 2, overloaded_500: 2 }, { success: 90, failed: 3, overloaded_500: 1 }]
  const original = structuredClone(buckets)
  assert.equal(accountHealthOverloadSummary(buckets).count, 3)
  assert.equal(accountHealthOverloadSummary(buckets).overloaded, true)
  assert.deepEqual(buckets, original)
})

test('plan count and range use exactly the health bar visible buckets', () => {
  const buckets = [{ success: 0, failed: 999, overloaded_500: 999 }]
  const start = Date.parse('2026-09-11T08:40:00Z')
  for (let index = 0; index < ACCOUNT_HEALTH_BLOCK_COUNT; index++) {
    buckets.push({ success: 50, failed: 1, overloaded_500: 1,
      start_at: new Date(start + index * 600_000).toISOString(), end_at: new Date(start + (index + 1) * 600_000).toISOString() })
  }
  const summary = accountHealthOverloadSummary(buckets)
  assert.equal(summary.count, 20)
  assert.equal(summary.minutes, ACCOUNT_HEALTH_BLOCK_COUNT * ACCOUNT_HEALTH_BLOCK_MINUTES)
  assert.equal(summary.minutes, 200)
  assert.equal(summary.startAt, start)
  assert.equal(summary.endAt, Date.parse('2026-09-11T12:00:00Z'))
  assert.equal(accountHealthOverloadSummary([{ success: 1, failed: 0, overloaded_500: 0 }]).overloaded, false)
})

test('invalid or missing counts and timestamps never invent an overload', () => {
  for (const overloaded_500 of [undefined, null, -1, 1.5, '5', NaN, Infinity]) {
    const summary = accountHealthOverloadSummary([{ success: 0, failed: 10, overloaded_500, start_at: 'invalid', end_at: 'invalid' }])
    assert.equal(summary.count, 0)
    assert.equal(summary.startAt, undefined)
    assert.equal(summary.endAt, undefined)
  }
})

test('table, personal and mobile plans share the health data and preserve workspace help', () => {
  const source = readFileSync(new URL('../pages/Accounts.tsx', import.meta.url), 'utf8')
  const badges = [...source.matchAll(/<PlanBadge\b[\s\S]*?\/>/g)]
  assert.equal(badges.length, 3)
  for (const [badge] of badges) assert.match(badge, /healthBuckets=\{healthBuckets\}/)
  const component = source.slice(source.indexOf('function PlanBadge('), source.indexOf('function getDefaultScoreBias('))
  assert.match(component, /accountHealthOverloadSummary\(healthBuckets\)/)
  assert.match(component, /overload\.overloaded[\s\S]*?bg-red-100/)
  assert.match(component, /accounts\.planOverloadCount/)
  assert.match(component, /count: overload\.count/)
  assert.match(component, /accounts\.planWorkspaceId/)
  const request = source.slice(source.indexOf('void api.getAccountHealthBars(ids)'))
  assert.match(request.slice(0, request.indexOf('useEffect(')), /\[accountPageIDsKey, data\.snapshotAt\]/)
})

test('health bar uses server time bounds and shared window constants', () => {
  const source = readFileSync(new URL('../components/AccountHealthBar.tsx', import.meta.url), 'utf8')
  assert.match(source, /blockCount = ACCOUNT_HEALTH_BLOCK_COUNT/)
  assert.match(source, /blockMinutes = ACCOUNT_HEALTH_BLOCK_MINUTES/)
  assert.match(source, /Date\.parse\(bucket\.start_at/)
  assert.match(source, /Date\.parse\(bucket\.end_at/)
})
