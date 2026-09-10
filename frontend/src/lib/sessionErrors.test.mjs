import assert from 'node:assert/strict'
import { test } from 'node:test'
import { selectableSessionKeys, validSessionSelection } from './sessionErrors.ts'
import { sessionErrorSearchParams } from '../api.ts'

test('session searches keep exact identity values encoded and never request other error statuses', () => {
  const query = new URLSearchParams(sessionErrorSearchParams({ userID: '17&user_id=18', sessionID: 'thread:0', lockedOnly: true, cursor: 'a+b/c' }))
  assert.equal(query.get('user_id'), '17&user_id=18')
  assert.equal(query.get('session_id'), 'thread:0')
  assert.equal(query.get('locked'), 'true')
  assert.equal(query.get('cursor'), 'a+b/c')
  assert.equal(query.has('status'), false)
})

test('batch locking cannot select locked descendants and unlocking only selects direct blacklist entries', () => {
  const items = [
    { identity: { key: 'open' }, locked: false },
    { identity: { key: 'direct' }, locked: true, locked_by: 'direct' },
    { identity: { key: 'child' }, locked: true, locked_by: 'direct' },
  ]
  assert.deepEqual(selectableSessionKeys(items, false), ['open'])
  assert.deepEqual(selectableSessionKeys(items, true), ['direct'])
  assert.deepEqual(validSessionSelection(['open', 'open', 'child', 'other-page'], items, false), ['open'])
  assert.deepEqual(validSessionSelection(['open', 'direct', 'child'], items, true), ['direct'])
})

test('session statistics default to unlocked and keep the persistent blacklist separate', () => {
  assert.equal(new URLSearchParams(sessionErrorSearchParams({})).get('lock_state'), 'unlocked')
  assert.equal(new URLSearchParams(sessionErrorSearchParams({ lockState: 'locked' })).get('lock_state'), 'locked')
  const blacklist = new URLSearchParams(sessionErrorSearchParams({ lockedOnly: true, lockState: 'unlocked' }))
  assert.equal(blacklist.get('locked'), 'true')
  assert.equal(blacklist.has('lock_state'), false)
})
