import { useTranslation } from 'react-i18next'
import type { UsageLog } from '@/types'

export default function UsageResponseModel({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  if (!log.upstream_response_model) return null
  return <div className="basis-full min-w-0 break-all text-[11px] text-muted-foreground" title={t('usage.responseModelHint')}>
    ↳ {t('usage.responseModel')}: {log.upstream_response_model}
  </div>
}
