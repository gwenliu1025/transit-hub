import type { ConnectionCapabilities } from '../types/mySites'

// 这里只消费后端的创建协议能力，不把模型厂商列表解释为协议白名单。
export const knownConnectionProtocol = (value: string | null | undefined, capabilities: ConnectionCapabilities | null): string => {
  const normalized = (value ?? '').trim().toLowerCase()
  return capabilities?.groupTypes?.some(option => option.id === normalized) ? normalized : ''
}

// 少数常见厂商名与协议名不同；只用于推荐排序，不参与创建校验。
const vendorProtocolAliases: Record<string, string> = { google: 'gemini', claude: 'anthropic' }

// New API 分组类型是逗号分隔的模型厂商列表，只保留能力中发布的协议，作为推荐而非限制。
export const inferredProtocols = (vendors: string | null | undefined, capabilities: ConnectionCapabilities | null): string[] => {
  const result: string[] = []
  for (const part of (vendors ?? '').split(',')) {
    const vendor = part.trim().toLowerCase()
    const protocol = knownConnectionProtocol(vendorProtocolAliases[vendor] ?? vendor, capabilities)
    if (protocol && !result.includes(protocol)) result.push(protocol)
  }
  return result
}

// 已选自有分组的唯一已知平台；未选、混选或平台未知时返回空字符串（Sub2API 账号只能绑定同平台分组）。
export const protocolFromOwnGroups = (
  selectedIds: readonly string[],
  groups: readonly { id: string; platform: string }[],
  capabilities: ConnectionCapabilities | null,
): string => {
  let protocol = ''
  for (const id of selectedIds) {
    const platform = knownConnectionProtocol(groups.find(group => group.id === id)?.platform, capabilities)
    if (!platform || (protocol && protocol !== platform)) return ''
    protocol = platform
  }
  return protocol
}

// 推荐平台排在前面，其余保持原顺序（Array.prototype.sort 为稳定排序）。
export const recommendedFirst = <T extends { platform: string }>(groups: readonly T[], recommended: readonly string[]): T[] => {
  const isRecommended = (group: T) => recommended.includes((group.platform ?? '').trim().toLowerCase())
  return [...groups].sort((a, b) => Number(isRecommended(b)) - Number(isRecommended(a)))
}

// Sub2API 上游的已知分组平台决定协议；New API 上游先选分组，显式选择优先，否则跟随所选分组的唯一平台。
export const effectiveConnectionProtocol = (
  platform: string | null | undefined,
  cached: string | null | undefined,
  selected: string,
  capabilities: ConnectionCapabilities | null,
  ownGroupProtocol = '',
): string => {
  const known = knownConnectionProtocol(cached, capabilities)
  if (platform !== 'newapi' && known) return known
  const explicit = knownConnectionProtocol(selected, capabilities)
  if (explicit || platform !== 'newapi') return explicit
  return knownConnectionProtocol(ownGroupProtocol, capabilities)
}

const safeConnectionErrors: Record<string, string> = {
  'auth.errors.unauthorized': 'admin.groupRates.connect.errors.localAuth',
  'admin.mySites.errors.authRequired': 'admin.groupRates.connect.errors.adminAuth',
  'admin.mySites.errors.adminOnly': 'admin.groupRates.connect.errors.adminOnly',
  'admin.adminAccounts.errors.noCurrentAccount': 'admin.groupRates.connect.errors.noCurrentAccount',
  'admin.mySites.errors.connectionExists': 'admin.groupRates.connect.errors.connectionExists',
  'admin.mySites.errors.connectionInProgress': 'admin.groupRates.connect.errors.connectionInProgress',
  'admin.mySites.errors.groupTypeRequired': 'admin.groupRates.connect.errors.groupTypeRequired',
  'admin.mySites.errors.groupTypeInvalid': 'admin.groupRates.connect.errors.groupTypeInvalid',
  'admin.mySites.errors.ownGroupPlatformMismatch': 'admin.groupRates.connect.errors.ownGroupPlatformMismatch',
  'admin.mySites.errors.channelTypeRequired': 'admin.groupRates.connect.errors.channelTypeRequired',
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
