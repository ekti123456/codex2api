import assert from 'node:assert/strict'
import { test } from 'node:test'
import { diagnosticClientInfo, diagnosticEntries, diagnosticJSONDisplay, diagnosticOutboundIdentity, diagnosticRecord, diagnosticValueText, usageRequestTypeLabelKey, usageRequestTypes } from './usageRequestDiagnostics.ts'

test('account mapping audit survives JSON display and copy without changing the input', () => {
  const mapping = { version: 'account-suffix-v1', status: 'mapped', changes: [{ original: 'original-id', outbound: 'account-id' }], cache_partitioned: true }
  const source = { format_version: 2, account_mapping: mapping, body: { client_metadata: { 'x-codex-turn-metadata': '{"session_id":"account-id"}' } } }
  const displayed = diagnosticJSONDisplay(source, true)
  assert.deepEqual(displayed.account_mapping, mapping)
  assert.deepEqual(JSON.parse(JSON.stringify(displayed)).account_mapping, mapping)
  assert.equal(source.body.client_metadata['x-codex-turn-metadata'], '{"session_id":"account-id"}')
})

test('JSON display preserves actual nesting and carrier types unless explicitly decoded', () => {
  const value = { format_version: 2, body: { prompt_cache_key: 'hash:cache', client_metadata: { 'x-codex-turn-metadata': '{"session_id":"session","window_number":2}' } } }
  assert.equal(diagnosticJSONDisplay(value), value)
  const decoded = diagnosticJSONDisplay(value, true)
  assert.deepEqual(decoded.body.client_metadata['x-codex-turn-metadata'], { session_id: 'session', window_number: 2 })
  assert.equal(typeof value.body.client_metadata['x-codex-turn-metadata'], 'string')
  assert.equal('links' in decoded.body, false)
  assert.equal('turn_metadata' in decoded.body, false)
  const old = { body: { turn_metadata: { session_id: 'old' }, links: { prompt_cache_key: 'hash:old' } } }
  assert.deepEqual(diagnosticJSONDisplay(old, true), old)
  assert.deepEqual(diagnosticJSONDisplay({ 'x-codex-turn-metadata': 'invalid' }, true), { 'x-codex-turn-metadata': 'invalid' })
})

test('historical and unknown request types are not inferred', () => {
  for (const value of usageRequestTypes) {
    assert.equal(usageRequestTypeLabelKey(value), `usage.diagnostics.types.${value}`)
  }
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

test('outbound identity separates actual handshake values from current frame values', () => {
  const info = diagnosticOutboundIdentity({
    http: { headers: { 'User-Agent': 'http-client' }, turn_metadata: { installation_id: 'http-device' } },
    ws_handshake: { headers: { 'User-Agent': 'original-handshake-client' }, turn_metadata: { window_id: 'thread:1' } },
    body: { client_metadata: { 'x-codex-installation-id': 'frame-device' }, turn_metadata: { window_id: 'thread:2' }, links: { previous_response_id: 'hash:response' }, input: 'not displayed' },
    incoming: { installation_id: 'not displayed' },
    _private: 'hidden',
  })
  assert.equal(info['http.headers.User-Agent'], 'http-client')
  assert.equal(info['ws_handshake.headers.User-Agent'], 'original-handshake-client')
  assert.equal(info['ws_handshake.turn_metadata.window_id'], 'thread:1')
  assert.equal(info['body.turn_metadata.window_id'], 'thread:2')
  assert.equal(info['body.client_metadata.x-codex-installation-id'], 'frame-device')
  assert.equal(info['body.links.previous_response_id'], 'hash:response')
  assert.equal(JSON.stringify(info).includes('not displayed'), false)
  assert.deepEqual(diagnosticOutboundIdentity(undefined), {})
  assert.deepEqual(diagnosticOutboundIdentity({ truncated: true }), { capture_truncated: true })
})

test('outbound session consistency displays recorded results without inferring historical values', () => {
  for (const result of ['matched', 'mismatched', 'missing_header', 'missing_body']) {
    assert.deepEqual(diagnosticOutboundIdentity({ session_consistency: result }), { session_consistency: result })
  }
  assert.deepEqual(diagnosticOutboundIdentity({ session_consistency: 'private-value' }), {})
  assert.deepEqual(diagnosticOutboundIdentity({ session_consistency: null }), {})
  assert.deepEqual(diagnosticOutboundIdentity({}), {})
})
