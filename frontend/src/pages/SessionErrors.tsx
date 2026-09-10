import { useCallback, useEffect, useRef, useState } from 'react'
import { LockKeyhole, RefreshCw, Search, UnlockKeyhole } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { selectableSessionKeys, validSessionSelection, type SessionErrorPage, type SessionErrorRow, type SessionErrorQuery } from '../lib/sessionErrors'
import { formatBeijingTime } from '../utils/time'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

const emptyPage: SessionErrorPage = { items: [], groups: 0, errors: 0, collector: { pending: 0, written: 0, dropped: 0, write_failures: 0, capacity: 512, retention_days: 7, max_rows: 100000 } }

export default function SessionErrors() {
  const { t } = useTranslation()
  const [filters, setFilters] = useState({ userID: '', sessionID: '', lockedOnly: false, lockState: 'unlocked' as NonNullable<SessionErrorQuery['lockState']>, cursors: [''] })
  const [draft, setDraft] = useState({ userID: '', sessionID: '' })
  const [selected, setSelected] = useState<string[]>([])
  const [confirming, setConfirming] = useState(false)
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState('')
  const [success, setSuccess] = useState('')
  const [detail, setDetail] = useState<SessionErrorRow | null>(null)
  const controller = useRef<AbortController | null>(null)
  const cursor = filters.cursors[filters.cursors.length - 1]
  const load = useCallback(async () => {
    controller.current?.abort()
    controller.current = new AbortController()
    return api.getSessionErrors({ userID: filters.userID, sessionID: filters.sessionID, lockedOnly: filters.lockedOnly, lockState: filters.lockState, cursor }, controller.current.signal)
  }, [filters.userID, filters.sessionID, filters.lockedOnly, filters.lockState, cursor])
  const { data, loading, error, reload, reloadSilently } = useDataLoader({ initialData: emptyPage, load })
  const canPoll = useRef(true)
  canPoll.current = selected.length === 0 && !confirming && !busy
  useEffect(() => () => controller.current?.abort(), [])
  useEffect(() => {
    if (cursor) return
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible' && canPoll.current) void reloadSilently()
    }, 30000)
    return () => window.clearInterval(timer)
  }, [cursor, reloadSilently])

  const unlockMode = filters.lockedOnly || filters.lockState === 'locked'
  const selection = validSessionSelection(selected, data.items, unlockMode)
  const eligible = selectableSessionKeys(data.items, unlockMode)
  const changeQuery = (next: typeof filters) => {
    setSelected([])
    setSuccess('')
    setActionError('')
    setFilters(next)
  }
  const updateBlacklist = async () => {
    if (busy || loading || error || selection.length === 0) return
    setBusy(true)
    setActionError('')
    try {
      await api.setSessionBlacklist(selection, !unlockMode)
      setSuccess(t(unlockMode ? 'sessionErrors.unlockedSuccess' : 'sessionErrors.lockedSuccess', { count: selection.length }))
      setSelected([])
      setConfirming(false)
      if (cursor) setFilters(current => ({ ...current, cursors: [''] }))
      else await reload()
    } catch (failure) {
      setActionError(failure instanceof Error ? failure.message : t('sessionErrors.updateFailed'))
    } finally {
      setBusy(false)
    }
  }

  return <>
    <PageHeader title={t('sessionErrors.title')} description={t('sessionErrors.description')} actions={
      <Button variant="outline" disabled={loading || busy} onClick={() => { setSelected([]); void reload() }}><RefreshCw className="size-4" />{t('common.refresh')}</Button>
    } />
    <Card className="mb-4 space-y-4 p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex gap-1" aria-label={t('sessionErrors.views')}>
          <Button variant={!filters.lockedOnly ? 'secondary' : 'ghost'} disabled={busy} aria-pressed={!filters.lockedOnly} onClick={() => changeQuery({ ...filters, lockedOnly: false, cursors: [''] })}>{t('sessionErrors.statistics')}</Button>
          <Button variant={filters.lockedOnly ? 'secondary' : 'ghost'} disabled={busy} aria-pressed={filters.lockedOnly} onClick={() => changeQuery({ ...filters, lockedOnly: true, cursors: [''] })}>{t('sessionErrors.blacklist')}</Button>
        </div>
        <p className="text-sm text-muted-foreground">{t('sessionErrors.summary', { groups: data.groups, count: data.errors })}</p>
      </div>
      <form className="flex flex-wrap gap-2" onSubmit={event => { event.preventDefault(); changeQuery({ ...filters, userID: draft.userID.trim(), sessionID: draft.sessionID.trim(), cursors: [''] }) }}>
        <Input className="w-48 max-sm:w-full" aria-label={t('sessionErrors.userSearch')} placeholder={t('sessionErrors.userSearch')} maxLength={255} value={draft.userID} onChange={event => setDraft({ ...draft, userID: event.target.value })} />
        <Input className="min-w-56 flex-1" aria-label={t('sessionErrors.sessionSearch')} placeholder={t('sessionErrors.sessionSearch')} maxLength={256} value={draft.sessionID} onChange={event => setDraft({ ...draft, sessionID: event.target.value })} />
        <Button type="submit" variant="outline" disabled={busy}><Search className="size-4" />{t('serviceErrors.search')}</Button>
      </form>
      <p className="text-xs leading-relaxed text-muted-foreground">{t('sessionErrors.retention')}</p>
      <p className="text-xs text-muted-foreground" role="status">{t('serviceErrors.collector', { pending: data.collector.pending, dropped: data.collector.dropped, failed: data.collector.write_failures })}</p>
      {(data.collector.dropped > 0 || data.collector.write_failures > 0) && <p role="alert" className="text-xs text-amber-600">{t('serviceErrors.lossWarning')}</p>}
    </Card>
    {success && <p role="status" className="mb-3 text-sm text-emerald-600">{success}</p>}
    <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
      <div className="flex flex-wrap items-center gap-3">
        {!filters.lockedOnly && <Select value={filters.lockState} disabled={busy || confirming} className="w-32" onValueChange={value => {
          if (value === 'unlocked' || value === 'locked') changeQuery({ ...filters, lockState: value, cursors: [''] })
        }} options={[{ value: 'unlocked', label: t('sessionErrors.allowed') }, { value: 'locked', label: t('sessionErrors.lockedFilter') }]} />}
        <span className="text-sm text-muted-foreground">{t('sessionErrors.selected', { count: selection.length })}</span>
      </div>
      <Button disabled={busy || loading || !!error || selection.length === 0} onClick={() => { setActionError(''); setConfirming(true) }}>
        {unlockMode ? <UnlockKeyhole className="size-4" /> : <LockKeyhole className="size-4" />}{t(unlockMode ? 'sessionErrors.unlockSelected' : 'sessionErrors.lockSelected')}
      </Button>
    </div>
    <StateShell loading={loading} error={error} onRetry={() => void reload()}>
      <Card className="overflow-hidden">
        {data.items.length === 0 ? <p className="p-10 text-center text-sm text-muted-foreground">{t('sessionErrors.empty')}</p> : <div className="overflow-x-auto"><Table className="min-w-[900px]">
          <TableHeader><TableRow>
            <TableHead className="w-10"><input type="checkbox" aria-label={t('sessionErrors.selectPage')} disabled={busy || eligible.length === 0} checked={eligible.length > 0 && selection.length === eligible.length} onChange={event => setSelected(event.target.checked ? eligible : [])} /></TableHead>
            {['user', 'session', 'account', 'count', 'last', 'error', 'status', 'details'].map(column => <TableHead key={column} title={column === 'account' ? t('sessionErrors.accountHint') : undefined}>{t(`sessionErrors.columns.${column}`)}</TableHead>)}
          </TableRow></TableHeader>
          <TableBody>{data.items.map(row => <TableRow key={row.identity.key}>
            <TableCell><input type="checkbox" aria-label={t('sessionErrors.selectSession', { user: row.identity.user_id, session: row.identity.session_id || row.identity.root_fingerprint })} checked={selection.includes(row.identity.key)} disabled={busy || !eligible.includes(row.identity.key)} onChange={event => setSelected(current => event.target.checked ? [...current, row.identity.key] : current.filter(key => key !== row.identity.key))} /></TableCell>
            <TableCell><p className="max-w-44 break-all text-sm">{row.identity.user_label || row.identity.user_id}</p><span className="text-xs text-muted-foreground">{row.identity.platform} · #{row.identity.user_id}</span></TableCell>
            <TableCell className="max-w-64 break-all font-mono text-xs">{row.identity.session_id || t('sessionErrors.sessionUnknown', { fingerprint: row.identity.root_fingerprint })}</TableCell>
            <TableCell className="max-w-56">{row.latest.account_id ? <>
              {(row.account_name || row.account_email) && <p className="truncate text-sm" title={row.account_name || row.account_email}>{row.account_name || row.account_email}</p>}
              <p className="truncate text-xs text-muted-foreground" title={row.account_email}>
                {row.account_name && row.account_email && row.account_name.toLowerCase() !== row.account_email.toLowerCase() ? `${row.account_email} · ` : ''}#{row.latest.account_id}
              </p>
            </> : '—'}</TableCell>
            <TableCell className="font-mono font-semibold">{row.count.toLocaleString()}</TableCell>
            <TableCell className="whitespace-nowrap text-xs">{row.count ? formatBeijingTime(row.last_at) : '—'}</TableCell>
            <TableCell className="max-w-64"><p className="truncate text-xs" title={row.latest.message}>{row.latest.code || '—'}</p><span className="text-xs text-muted-foreground">{row.latest.model}</span></TableCell>
            <TableCell><Badge variant="outline">{t(row.lineage_invalid ? 'sessionErrors.lineageInvalid' : !row.locked ? 'sessionErrors.allowed' : row.locked_by === row.identity.key ? 'sessionErrors.locked' : 'sessionErrors.inherited')}</Badge></TableCell>
            <TableCell><Button size="sm" variant="ghost" onClick={() => setDetail(row)}>{t('sessionErrors.columns.details')}</Button></TableCell>
          </TableRow>)}</TableBody>
        </Table></div>}
      </Card>
    </StateShell>
    <div className="mt-4 flex justify-between gap-2">
      <span className="text-sm text-muted-foreground">{t('serviceErrors.page', { page: filters.cursors.length })}</span>
      <div className="flex gap-2">
        <Button variant="outline" disabled={busy || loading || !cursor} onClick={() => changeQuery({ ...filters, cursors: filters.cursors.slice(0, -1) })}>{t('common.prev')}</Button>
        <Button variant="outline" disabled={busy || loading || !!error || !data.next_cursor} onClick={() => { if (data.next_cursor) changeQuery({ ...filters, cursors: [...filters.cursors, data.next_cursor] }) }}>{t('common.next')}</Button>
      </div>
    </div>
    <Dialog open={confirming} onOpenChange={open => { if (!busy) setConfirming(open) }}><DialogContent>
      <DialogHeader><DialogTitle>{t(unlockMode ? 'sessionErrors.confirmUnlock' : 'sessionErrors.confirmLock', { count: selection.length })}</DialogTitle><DialogDescription>{t(unlockMode ? 'sessionErrors.unlockHint' : 'sessionErrors.lockHint')}</DialogDescription></DialogHeader>
      <ul className="max-h-48 overflow-y-auto space-y-2 text-xs">{data.items.filter(row => selection.includes(row.identity.key)).map(row => <li className="break-all font-mono" key={row.identity.key}>{row.identity.platform} / {row.identity.user_id} / {row.identity.session_id || row.identity.root_fingerprint}</li>)}</ul>
      {actionError && <p role="alert" className="text-sm text-destructive">{actionError}</p>}
      <DialogFooter><Button variant="outline" disabled={busy} onClick={() => setConfirming(false)}>{t('common.cancel')}</Button><Button disabled={busy || selection.length === 0} onClick={() => void updateBlacklist()}>{t(busy ? 'sessionErrors.saving' : 'common.confirm')}</Button></DialogFooter>
    </DialogContent></Dialog>
    <Dialog open={!!detail} onOpenChange={open => { if (!open) setDetail(null) }}><DialogContent className="max-w-3xl">
      <DialogHeader><DialogTitle>{t('sessionErrors.columns.details')}</DialogTitle><DialogDescription>{t('sessionErrors.detailHint')}</DialogDescription></DialogHeader>
      <pre className="max-h-[65vh] overflow-auto whitespace-pre-wrap break-all rounded-md bg-muted p-3 text-xs">{JSON.stringify(detail, null, 2)}</pre>
    </DialogContent></Dialog>
  </>
}
