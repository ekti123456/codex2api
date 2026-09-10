export const SERVICE_ERROR_STAGES = ['authentication', 'rate_limit', 'root_binding', 'window', 'policy', 'dispatch', 'validation', 'internal'] as const

export interface ServiceErrorEvent {
  id: string
  created_at: string
  request_id: string
  newapi_request_id?: string
  newapi_identity_verified?: boolean
  status_code: number
  code: string
  error_type: string
  message: string
  stage: string
  method: string
  endpoint: string
  transport: string
  model?: string
  duration_ms: number
  api_key_id?: number
  api_key_name?: string
  request_type: string
  thread_source?: string
  request_kind?: string
  subagent_kind?: string
  thread_id?: string
  window_id?: string
  root_fingerprint?: string
  scope_hash?: string
  root_account_lookup?: string
  root_account_wait?: string
  root_account_wait_ms?: number
  candidate_rejections?: string[]
  client_info?: Record<string, string>
}

export interface ServiceErrorPage {
  items: ServiceErrorEvent[]
  next_cursor?: string
  summary: { total: number; status_429: number; status_4xx: number; status_5xx: number }
  collector: { pending: number; written: number; dropped: number; write_failures: number; capacity: number; retention_days: number; max_rows: number }
}

export interface ServiceErrorQuery {
  start: string
  end: string
  status?: string
  stage?: string
  request_id?: string
  cursor?: string
}

export function serviceErrorCollectorHasLoss(collector: ServiceErrorPage['collector']): boolean {
  return collector.dropped > 0 || collector.write_failures > 0
}
