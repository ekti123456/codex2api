import { useTranslation } from 'react-i18next'
import type { AccountHealthLatestRequest } from '../types'
import { accountLatestTurnStateValue } from '../lib/accountHealth'

export default function AccountLatestTurnState({ request }: { request?: AccountHealthLatestRequest }) {
  const { t } = useTranslation()
  const value = accountLatestTurnStateValue(request)
  if (!value) return null
  const label = value.length !== undefined
    ? t('usage.turnState.characters', { count: value.length })
    : t(`usage.turnState.${value.state}`)
  return <span className="mt-1 block border-t border-current/15 pt-1 text-[11px] first:mt-0 first:border-0 first:pt-0">
    {t('accounts.healthBarTurnState', { value: label })}
  </span>
}
