export interface UsageRequestDiagnosticDetail {
  request_type: string
  diagnostics: Record<string, unknown> | null
}

export const usageRequestTypes = ['user', 'related_internal', 'independent_internal', 'related_unclassified', 'compaction', 'gateway_internal', 'unknown'] as const
const requestTypes = new Set<string>(usageRequestTypes)

export function usageRequestTypeLabelKey(value?: string): string {
  if (!value) return 'usage.diagnostics.types.not_recorded'
  return `usage.diagnostics.types.${requestTypes.has(value) ? value : 'unknown'}`
}

export function diagnosticRecord(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {}
}

const clientMetadataFields = new Set([
  'installation_id', 'installationId', 'device_id', 'deviceId',
  'x-codex-installation-id', 'x_codex_installation_id', 'x-device-id', 'x_device_id',
  'client_name', 'client_version', 'os_name', 'os_version', 'arch', 'timezone',
])

const clientHeaderFields = new Set([
  'X-Codex-Installation-Id', 'X-Installation-Id', 'X-Device-Id', 'Oai-Device-Id',
  'User-Agent', 'Originator', 'Version', 'X-Stainless-OS', 'X-Stainless-Arch',
  'X-Stainless-Runtime', 'X-Stainless-Runtime-Version', 'X-Stainless-Package-Version',
])

export function diagnosticClientInfo(incoming: unknown): Record<string, unknown> {
  const result: Record<string, unknown> = {}
  for (const [source, values] of Object.entries(diagnosticRecord(incoming))) {
    if (source.startsWith('_')) continue
    const allowed = source === 'headers' ? clientHeaderFields : clientMetadataFields
    for (const [field, value] of Object.entries(diagnosticRecord(values))) {
      if (allowed.has(field)) result[`${source}.${field}`] = value
    }
  }
  return result
}

export function diagnosticEntries(value: unknown, includeMissing = false): [string, unknown][] {
  const record = diagnosticRecord(value)
  const fields = includeMissing
    ? { thread_source: '', request_kind: '', subagent_kind: '', session_id: '', thread_id: '', parent_thread_id: '', turn_id: '', root_turn_id: '', ...record }
    : record
  return Object.entries(fields).filter(([key]) => !key.startsWith('_'))
}

export function diagnosticOutboundIdentity(value: unknown): Record<string, unknown> {
  const identity = diagnosticRecord(value)
  const result: Record<string, unknown> = {}
  if (identity.truncated === true) result.capture_truncated = true
  if (typeof identity.session_consistency === 'string' && ['matched', 'mismatched', 'missing_header', 'missing_body'].includes(identity.session_consistency)) {
    result.session_consistency = identity.session_consistency
  }
  for (const source of ['http', 'ws_handshake', 'body']) {
    const groups = diagnosticRecord(identity[source])
    const names = source === 'body' ? ['client_metadata', 'turn_metadata', 'links'] : ['headers', 'turn_metadata']
    for (const group of names) {
      for (const [field, item] of diagnosticEntries(groups[group])) {
        result[`${source}.${group}.${field}`] = item
      }
    }
  }
  return result
}

export function diagnosticValueText(value: unknown): string | null {
  if (value === null || value === undefined || value === '' || value === '0001-01-01T00:00:00Z') return null
  if (typeof value === 'string') return value
  if (typeof value === 'boolean' || typeof value === 'number') return String(value)
  if (Array.isArray(value)) return value.length ? value.map(String).join(', ') : null
  return JSON.stringify(value)
}

export function diagnosticJSONDisplay(value: unknown, decodeMetadata = false, depth = 0): unknown {
  if (!decodeMetadata || depth > 24 || value === null || typeof value !== 'object') return value
  if (Array.isArray(value)) return value.map((item) => diagnosticJSONDisplay(item, true, depth + 1))
  return Object.fromEntries(Object.entries(value).map(([key, item]) => {
    if (key.toLowerCase() === 'x-codex-turn-metadata' && typeof item === 'string' && item.length <= 16384) {
      try {
        const decoded: unknown = JSON.parse(item)
        if (decoded !== null && typeof decoded === 'object' && !Array.isArray(decoded)) return [key, decoded]
      } catch {
        return [key, item]
      }
    }
    return [key, diagnosticJSONDisplay(item, true, depth + 1)]
  }))
}
