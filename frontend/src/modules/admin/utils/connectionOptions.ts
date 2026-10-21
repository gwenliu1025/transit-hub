import type { ConnectionCapabilities } from '../types/mySites'

// 这里只消费后端的创建协议能力，不把模型厂商列表解释为协议白名单。
export const knownConnectionProtocol = (value: string | null | undefined, capabilities: ConnectionCapabilities | null): string => {
  const normalized = (value ?? '').trim().toLowerCase()
  return capabilities?.groupTypes?.some(option => option.id === normalized) ? normalized : ''
}

export const effectiveConnectionProtocol = (platform: string | null | undefined, cached: string | null | undefined, selected: string, capabilities: ConnectionCapabilities | null): string => {
  const known = knownConnectionProtocol(cached, capabilities)
  if (platform !== 'newapi' && known) return known
  return knownConnectionProtocol(selected, capabilities)
}

const safeConnectionErrors: Record<string, string> = {
  'auth.errors.unauthorized': 'admin.groupRates.connect.errors.localAuth',
  'admin.mySites.errors.authRequired': 'admin.groupRates.connect.errors.adminAuth',
  'admin.mySites.errors.adminOnly': 'admin.groupRates.connect.errors.adminOnly',
  'admin.adminAccounts.errors.noCurrentAccount': 'admin.groupRates.connect.errors.noCurrentAccount',
  'admin.mySites.errors.connectionExists': 'admin.groupRates.connect.errors.connectionExists',
  'admin.mySites.errors.groupTypeRequired': 'admin.groupRates.connect.errors.groupTypeRequired',
  'admin.mySites.errors.groupTypeInvalid': 'admin.groupRates.connect.errors.groupTypeInvalid',
  'admin.mySites.errors.network': 'admin.groupRates.connect.errors.network',
  'admin.upstream.errors.invalidUrl': 'admin.groupRates.connect.errors.invalidUrl',
  'admin.upstream.errors.auth': 'admin.groupRates.connect.errors.remoteAuth',
  'admin.upstream.errors.network': 'admin.groupRates.connect.errors.network',
  'admin.upstream.errors.request': 'admin.groupRates.connect.errors.remoteRequest',
  'admin.upstream.errors.invalidResponse': 'admin.groupRates.connect.errors.invalidResponse',
  'admin.upstream.errors.notFound': 'admin.groupRates.connect.errors.notFound',
}

// 任意远端正文、网络异常与未知键都不会传给 i18n 或直接展示。
export const connectionErrorKey = (error: unknown, fallback: string): string => {
  if (!(error instanceof Error)) return fallback
  return Object.prototype.hasOwnProperty.call(safeConnectionErrors, error.message) ? safeConnectionErrors[error.message]! : fallback
}
