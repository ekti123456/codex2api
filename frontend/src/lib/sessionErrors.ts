import type { ServiceErrorPage } from './serviceErrors'

export interface SessionErrorIdentity {
  key: string
  kind: string
  platform: string
  user_id: string
  user_label?: string
  session_id?: string
  root_fingerprint?: string
  parent_key?: string
}

export interface SessionErrorRow {
  identity: SessionErrorIdentity
  count: number
  first_at: string
  last_at: string
  latest: { request_id: string; newapi_request_id?: string; code: string; message: string; model?: string; account_id?: number; transport: string; endpoint: string }
  locked: boolean
  locked_by?: string
  lineage_invalid?: boolean
}

export interface SessionErrorPage {
  items: SessionErrorRow[]
  groups: number
  errors: number
  next_cursor?: string
  collector: ServiceErrorPage['collector']
}

export interface SessionErrorQuery {
  userID?: string
  sessionID?: string
  lockedOnly?: boolean
  cursor?: string
}

export function selectableSessionKeys(items: SessionErrorRow[], lockedOnly: boolean): string[] {
  return items.filter(item => lockedOnly ? item.locked_by === item.identity.key : !item.locked).map(item => item.identity.key)
}

export function validSessionSelection(selected: string[], items: SessionErrorRow[], lockedOnly: boolean): string[] {
  const eligible = new Set(selectableSessionKeys(items, lockedOnly))
  return [...new Set(selected)].filter(key => eligible.has(key)).slice(0, 100)
}
