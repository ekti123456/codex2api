export async function writeClipboardText(text: string, container?: HTMLElement | null): Promise<void> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return
    } catch {
    }
  }

  const ownerDocument = container?.ownerDocument ?? document
  const activeElement = ownerDocument.activeElement
  const textarea = ownerDocument.createElement('textarea')
  textarea.value = text
  textarea.setAttribute('readonly', '')
  textarea.style.position = 'fixed'
  textarea.style.opacity = '0'
  textarea.style.pointerEvents = 'none'
  const target = container ?? ownerDocument.body
  target.appendChild(textarea)
  try {
    textarea.focus({ preventScroll: true })
    textarea.select()
    if (!ownerDocument.execCommand('copy')) throw new Error('Clipboard write failed')
  } finally {
    textarea.remove()
    if (activeElement instanceof HTMLElement) activeElement.focus({ preventScroll: true })
  }
}
