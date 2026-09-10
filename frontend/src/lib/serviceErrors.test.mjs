import assert from 'node:assert/strict'
import test from 'node:test'
import { serviceErrorCollectorHasLoss } from './serviceErrors.ts'
import { serviceErrorSearchParams } from '../api.ts'

test('service error filters preserve exact request IDs and opaque cursors', () => {
  const query = new URLSearchParams(serviceErrorSearchParams({ start: '2026-09-10T00:00:00Z', end: '2026-09-10T01:00:00Z', status: '429', stage: 'rate_limit', request_id: ' req+id&x=1 ', cursor: 'opaque/cursor+=' }))
  assert.equal(query.get('request_id'), 'req+id&x=1')
  assert.equal(query.get('cursor'), 'opaque/cursor+=')
  assert.equal(query.get('limit'), '20')
  assert.equal(query.get('stage'), 'rate_limit')
  assert.equal(query.has('x'), false)
})

test('service error empty filters are omitted and collector loss is visible', () => {
  const query = new URLSearchParams(serviceErrorSearchParams({ start: 'start', end: 'end', status: '', cursor: '' }))
  assert.equal(query.has('cursor'), false)
  assert.equal(query.has('status'), false)
  assert.equal(serviceErrorCollectorHasLoss({ dropped: 0, write_failures: 0 }), false)
  assert.equal(serviceErrorCollectorHasLoss({ dropped: 1, write_failures: 0 }), true)
  assert.equal(serviceErrorCollectorHasLoss({ dropped: 0, write_failures: 1 }), true)
})
