import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '@/api'
import { Card } from '@/components/ui/card'
import { Switch } from '@/components/ui/switch'
import { Input } from '@/components/ui/input'
import { Button } from '@/components/ui/button'

export default function SessionAutoLockSettings() {
  const { t } = useTranslation()
  const [enabled, setEnabled] = useState(false)
  const [threshold, setThreshold] = useState('3')
  const [ready, setReady] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [saved, setSaved] = useState(false)
  const [reload, setReload] = useState(0)
  useEffect(() => {
    const controller = new AbortController()
    setReady(false)
    setError('')
    api.getSessionAutoLockSettings(controller.signal).then(value => {
      if (controller.signal.aborted) return
      setEnabled(value.enabled); setThreshold(String(value.threshold)); setReady(true)
    }).catch(reason => { if (!controller.signal.aborted) setError(String(reason)) })
    return () => controller.abort()
  }, [reload])
  const count = Number(threshold)
  const valid = /^\d+$/.test(threshold) && Number.isInteger(count) && count >= 1 && count <= 10000
  const save = async () => {
    if (!ready || saving || !valid) return
    setSaving(true); setError(''); setSaved(false)
    try {
      const value = await api.setSessionAutoLockSettings({ enabled, threshold: count })
      setEnabled(value.enabled); setThreshold(String(value.threshold)); setSaved(true)
    } catch (reason) { setError(String(reason)) }
    finally { setSaving(false) }
  }
  return <Card className="mb-4 space-y-3 p-4">
    <div className="flex flex-wrap items-center gap-4">
      <label className="flex items-center gap-2 text-sm font-medium">
        <Switch checked={enabled} disabled={!ready || saving} onCheckedChange={value => { setEnabled(value); setSaved(false) }} />
        {t('sessionErrors.autoLockTitle')}
      </label>
      <label className="flex items-center gap-2 text-sm">
        {t('sessionErrors.autoLockThreshold')}
        <Input className="w-24" type="number" min={1} max={10000} step={1} value={threshold} disabled={!ready || saving} onChange={event => { setThreshold(event.target.value); setSaved(false) }} />
      </label>
      <Button size="sm" disabled={!ready || saving || !valid} onClick={() => void save()}>{t(saving ? 'sessionErrors.saving' : 'common.save')}</Button>
      {saved && <span role="status" className="text-sm text-emerald-600">{t('sessionErrors.autoLockSaved')}</span>}
    </div>
    <p className="text-xs leading-relaxed text-muted-foreground">{t('sessionErrors.autoLockHint')}</p>
    {error && <div role="alert" className="flex items-center gap-3 text-sm text-destructive">{error}{!ready && <Button size="sm" variant="outline" onClick={() => setReload(value => value + 1)}>{t('common.refresh')}</Button>}</div>}
  </Card>
}
