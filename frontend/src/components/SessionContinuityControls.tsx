import { useTranslation } from 'react-i18next'
import { Select } from './ui/select'

export default function SessionContinuityControls(props: {
  mode: string
  onChange: (mode: string) => void
}) {
  const { t } = useTranslation()
  return <section aria-labelledby="session-continuity-title" className="mt-4 space-y-3 rounded-lg border border-primary/20 bg-primary/[0.03] p-3">
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div id="session-continuity-title" className="text-sm font-semibold">{t('sessionContinuity.title')}</div>
      <Select value={props.mode || 'observe'} onValueChange={(mode) => {
        if (mode === 'enforce' && !window.confirm(t('sessionContinuity.confirm'))) return
        props.onChange(mode)
      }} options={['off', 'observe', 'enforce'].map((mode) => ({ value: mode, label: t(`sessionContinuity.${mode}`) }))} />
    </div>
    <p className="text-xs leading-relaxed text-muted-foreground">{t('sessionContinuity.description')}</p>
    <p className="text-xs leading-relaxed text-muted-foreground">{t('sessionContinuity.protection')}</p>
  </section>
}
