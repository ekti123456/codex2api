import assert from 'node:assert/strict'
import { test } from 'node:test'
import { readFileSync } from 'node:fs'
import { defaultSessionCreationCooldown, parseSessionCreationCooldown, sessionCreationCooldownInterval } from './sessionCreationCooldown.ts'

test('creation cooldown defaults and inclusive boundaries match the server policy', () => {
  const config = defaultSessionCreationCooldown()
  assert.equal(config.mode, 'off')
  assert.equal(config.frequency_window_seconds, 1800)
  assert.equal(config.free_creations, 2)
  config.mode = 'enforce'
  for (const [duration, expected] of [[0, 900], [299, 900], [300, 600], [599, 600], [600, 300], [899, 300], [900, 0]]) {
    assert.equal(sessionCreationCooldownInterval(config, duration, 10), expected)
  }
  assert.equal(sessionCreationCooldownInterval(config, 0, 9), 0)
  config.tiers[0].interval_seconds = 123
  assert.equal(defaultSessionCreationCooldown().tiers[0].interval_seconds, 0)
  assert.equal(parseSessionCreationCooldown({ free_creations: 4 }).free_creations, 4)
})

test('creation cooldown controls stay separate from quantity switch and have translated labels', () => {
  const page = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
  assert.match(page, /SessionCreationCooldownControls value=\{config\.risk\.session_creation_cooldown\}/)
  for (const locale of ['zh', 'zh-TW', 'en']) {
    const content = JSON.parse(readFileSync(new URL(`../locales/${locale}.json`, import.meta.url), 'utf8'))
    for (const label of ['title', 'off', 'observe', 'enforce', 'frequencyWindow', 'freeCreations', 'historyDays', 'minSamples', 'maxSamples', 'maxInterval', 'lowerBound', 'interval', 'windowLimitDelta', 'windowLimitHint', 'addTier', 'reset']) {
      assert.ok(content.promptFilter.creationCooldown[label], `${locale}: ${label}`)
    }
  }
})

test('window adjustments default to zero and retain signed values across editing and JSON round trips', () => {
  assert.ok(defaultSessionCreationCooldown().tiers.every((tier) => tier.window_limit_delta === 0))
  const legacy = parseSessionCreationCooldown({ tiers: [{ min_average_seconds: 0, interval_seconds: 300 }] })
  assert.equal(legacy.tiers[0].window_limit_delta, 0)
  for (const adjustment of [-1, 0, 1]) {
    const config = parseSessionCreationCooldown({ tiers: [{ min_average_seconds: 0, interval_seconds: 300, window_limit_delta: adjustment }] })
    assert.equal(parseSessionCreationCooldown(JSON.parse(JSON.stringify(config))).tiers[0].window_limit_delta, adjustment)
    assert.equal(config.tiers[0].interval_seconds, 300)
  }
})
