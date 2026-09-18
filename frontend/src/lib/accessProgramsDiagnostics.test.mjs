import assert from 'node:assert/strict'
import { test } from 'node:test'
import { accessProgramsDiagnosticValue } from './usageRequestDiagnostics.ts'

test('access_programs distinguishes absence, explicit values and missing snapshots', () => {
  for (const value of [null, {}, [], '', false, 42, { cyber: 'daybreak_blue' }]) {
    assert.deepEqual(accessProgramsDiagnosticValue({ state: 'present', value }), {
      state: 'present', text: JSON.stringify(value, null, 2),
    })
  }
  assert.deepEqual(accessProgramsDiagnosticValue({ state: 'absent' }), { state: 'absent' })
  assert.deepEqual(accessProgramsDiagnosticValue({ state: 'invalid_json' }), { state: 'invalid_json' })
  for (const snapshot of [undefined, null, {}, { state: 'present' }]) {
    assert.deepEqual(accessProgramsDiagnosticValue(snapshot), { state: 'not_recorded' })
  }
})

test('access_programs shows oversized evidence without inventing a value', () => {
  const rendered = accessProgramsDiagnosticValue({ state: 'too_large', bytes: 2048, sha256: 'digest' })
  assert.equal(rendered.state, 'too_large')
  assert.deepEqual(JSON.parse(rendered.text), { bytes: 2048, sha256: 'digest' })
})
