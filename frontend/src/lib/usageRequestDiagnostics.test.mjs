import assert from 'node:assert/strict'
import { test } from 'node:test'
import { diagnosticClientInfo, diagnosticEntries, diagnosticRecord, diagnosticValueText, usageRequestTypeLabelKey } from './usageRequestDiagnostics.ts'

test('historical and unknown request types are not inferred', () => {
  assert.equal(usageRequestTypeLabelKey(), 'usage.diagnostics.types.not_recorded')
  assert.equal(usageRequestTypeLabelKey(''), 'usage.diagnostics.types.not_recorded')
  assert.equal(usageRequestTypeLabelKey('future'), 'usage.diagnostics.types.unknown')
  assert.equal(usageRequestTypeLabelKey('related_internal'), 'usage.diagnostics.types.related_internal')
})

test('diagnostic values distinguish missing from false and zero', () => {
  for (const value of [null, undefined, '', '0001-01-01T00:00:00Z', []]) assert.equal(diagnosticValueText(value), null)
  assert.equal(diagnosticValueText(false), 'false')
  assert.equal(diagnosticValueText(0), '0')
  assert.equal(diagnosticValueText(['concurrency', 'model']), 'concurrency, model')
})

test('diagnostic fields show missing source values and omit private state', () => {
  const entries = Object.fromEntries(diagnosticEntries({ thread_source: 'guardian_review', _internal: 'hidden', passive_authorized: false }, true))
  assert.equal(entries.thread_source, 'guardian_review')
  assert.equal(entries.request_kind, '')
  assert.equal(entries.passive_authorized, false)
  assert.equal('_internal' in entries, false)
  for (const value of [null, [], 'text', 17]) assert.deepEqual(diagnosticRecord(value), {})
})

test('device diagnostics retain distinct sources without guessing a winning installation ID', () => {
  const info = diagnosticClientInfo({
    headers: { 'X-Codex-Installation-Id': 'header-device', 'User-Agent': 'codex-tui/0.1', Authorization: 'secret' },
    turn_metadata_header: { installation_id: 'metadata-device', client_version: '0.1', prompt: 'private' },
    signed_newapi: { installation_id: 'signed-device', token_id: '12' },
    'metadata.user_id': { device_id: 'hash:claude-device', session_id: 'session' },
    _private: { installation_id: 'hidden' },
  })
  assert.equal(info['headers.X-Codex-Installation-Id'], 'header-device')
  assert.equal(info['turn_metadata_header.installation_id'], 'metadata-device')
  assert.equal(info['signed_newapi.installation_id'], 'signed-device')
  assert.equal(info['metadata.user_id.device_id'], 'hash:claude-device')
  assert.equal(info['headers.User-Agent'], 'codex-tui/0.1')
  assert.equal(Object.keys(info).some(key => key.includes('Authorization') || key.includes('prompt') || key.startsWith('_')), false)
  assert.equal(Object.hasOwn(info, 'installation_id'), false)
})

test('old diagnostic snapshots do not synthesize device information', () => {
  assert.deepEqual(diagnosticClientInfo(undefined), {})
  assert.deepEqual(diagnosticClientInfo({ signed_newapi: { root_fingerprint: 'root' } }), {})
  const fields = Object.fromEntries(diagnosticEntries({ window_number: 0 }, true))
  assert.equal(fields.window_number, 0)
  assert.equal(diagnosticValueText(fields.installation_id), null)
})
