import { useTranslation } from 'react-i18next'
import { Badge } from './ui/badge'
import { Tooltip, TooltipContent, TooltipTrigger } from './ui/tooltip'
import type { UsageLog } from '../types'

export function UsageTurnStartBadge({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  if (log.is_turn_first_request !== true || !log.turn_prompt_preview) return null

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge
          variant="outline"
          tabIndex={0}
          aria-label={`${t('usage.turnStartBadge')}: ${log.turn_prompt_preview}`}
          className="ml-1.5 cursor-default border-transparent bg-sky-500/10 px-1.5 py-0 align-middle font-sans text-[11px] text-sky-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring dark:text-sky-300"
        >
          {t('usage.turnStartBadge')}
        </Badge>
      </TooltipTrigger>
      <TooltipContent className="max-w-xs break-words">{log.turn_prompt_preview}</TooltipContent>
    </Tooltip>
  )
}
