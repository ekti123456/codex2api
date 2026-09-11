import assert from 'node:assert/strict'
import test from 'node:test'
import { writeClipboardText } from './clipboard.ts'

for (const mode of ['modern', 'missing api', 'denied then fallback', 'both denied', 'fallback throws']) {
  test(`clipboard ${mode} preserves text and cleans up the dialog fallback`, async context => {
    const original = new Map(['navigator', 'document', 'HTMLElement'].map(key => [key, Object.getOwnPropertyDescriptor(globalThis, key)]))
    context.after(() => {
      for (const [key, descriptor] of original) {
        if (descriptor) Object.defineProperty(globalThis, key, descriptor)
        else delete globalThis[key]
      }
    })
    let copied = ''
    class Element {
      style = {}
      children = []
      value = ''
      constructor(document) { this.ownerDocument = document }
      appendChild(child) { this.children.push(child); child.parent = this }
      remove() { this.parent.children = this.parent.children.filter(child => child !== this) }
      setAttribute() {}
      focus() { this.ownerDocument.activeElement = this }
      select() { this.selected = true }
    }
    const document = {
      createElement: name => { assert.equal(name, 'textarea'); return new Element(document) },
      execCommand: command => {
        assert.equal(command, 'copy')
        const textarea = document.activeElement
        assert.equal(textarea.parent, container)
        assert.equal(textarea.selected, true)
        if (mode === 'fallback throws') throw new Error('copy unavailable')
        if (mode === 'both denied') return false
        copied = textarea.value
        return true
      },
    }
    document.body = new Element(document)
    const container = new Element(document)
    const button = new Element(document)
    document.activeElement = button
    const clipboard = mode === 'missing api' ? undefined : { writeText: async value => {
      if (mode !== 'modern') throw new Error('permission denied')
      copied = value
    } }
    for (const [key, value] of Object.entries({ navigator: { clipboard }, document, HTMLElement: Element })) {
      Object.defineProperty(globalThis, key, { configurable: true, value })
    }
    const json = '{"message":"窗口已满","nested":{"count":2}}'
    if (mode === 'both denied' || mode === 'fallback throws') {
      await assert.rejects(writeClipboardText(json, container))
      assert.equal(copied, '')
    } else {
      await writeClipboardText(json, container)
      assert.equal(copied, json)
    }
    assert.equal(document.activeElement, button)
    assert.equal(container.children.length, 0)
    assert.equal(document.body.children.length, 0)
  })
}
