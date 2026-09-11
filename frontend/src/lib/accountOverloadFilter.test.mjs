import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { api } from '../api.ts'

test('account overload filters are sent with page and other filters, including bulk selectors', async context => {
  const storage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, value: { getItem: () => '' } })
  context.after(() => {
    if (storage) Object.defineProperty(globalThis, 'localStorage', storage)
    else delete globalThis.localStorage
  })
  const requests = []
  context.mock.method(globalThis, 'fetch', async (url, options) => {
    requests.push({ url: new URL(url, 'http://localhost'), options })
    return new Response('{}', { headers: { 'Content-Type': 'application/json' } })
  })
  for (const overload500 of ['all', 'marked', 'unmarked']) {
    await api.getAccountsPage({ channel: 'codex', page: 2, pageSize: 20, status: 'disabled', search: 'example', overload500 })
    const query = requests.at(-1).url.searchParams
    assert.equal(query.get('overload_500'), overload500 === 'all' ? null : overload500)
    assert.equal(query.get('status'), 'disabled')
    assert.equal(query.get('page'), '2')
    assert.equal(query.get('search'), 'example')
  }
  await api.batchTestAccounts({ channel: 'codex', overload_500: 'marked', status: 'disabled' })
  assert.deepEqual(JSON.parse(requests.at(-1).options.body), {
    selector: { channel: 'codex', overload_500: 'marked', status: 'disabled' },
  })
})

test('account overload filter defaults to all and joins the server-side selector', () => {
  const source = readFileSync(new URL('../pages/Accounts.tsx', import.meta.url), 'utf8')
  assert.match(source, /\[overload500Filter, setOverload500Filter\][^\n]+\("all"\)/)
  assert.match(source, /overload500: overload500Filter/)
  assert.match(source, /overload_500: overload500Filter === "all" \? undefined : overload500Filter/)
  assert.match(source, /setOverload500Filter\(key\);\s*setPage\(1\)/)
  assert.match(source, /setPlanFilter\("all"\);\s*setOverload500Filter\("all"\)/)
  for (const locale of ['zh', 'zh-TW', 'en']) {
    const translations = JSON.parse(readFileSync(new URL(`../locales/${locale}.json`, import.meta.url), 'utf8'))
    for (const key of ['overload500Filter', 'overload500Marked', 'overload500Unmarked', 'overload500FilterHint']) {
      assert.equal(typeof translations.accounts[key], 'string')
      assert.ok(translations.accounts[key].length > 0)
    }
  }
})
