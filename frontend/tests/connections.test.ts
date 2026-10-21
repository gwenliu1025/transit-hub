import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { test } from 'node:test'
import { parse, compileScript } from '@vue/compiler-sfc'
import ts from 'typescript'
import * as vue from 'vue'
import { connectionErrorKey, effectiveConnectionProtocol } from '../src/modules/admin/utils/connectionOptions.ts'

const require = createRequire(import.meta.url)
const read = (path: string) => readFileSync(new URL(path, import.meta.url), 'utf8')
const evaluate = (source: string, dependencies: Record<string, any> = {}) => {
  const output = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText
  const module = { exports: {} as any }
  new Function('require', 'module', 'exports', output)((id: string) => dependencies[id] ?? require(id), module, module.exports)
  return module.exports
}
const componentSource = compileScript(parse(read('../src/modules/admin/views/GroupRatesView.vue')).descriptor, { id: 'connection-tests' }).content
const types = evaluate(read('../src/modules/admin/types/mySites.ts'))
const protocolOptions = [{ id: 'openai', name: 'OpenAI' }, { id: 'anthropic', name: 'Anthropic' }, { id: 'gemini', name: 'Gemini' }, { id: 'antigravity', name: 'Antigravity' }]
const capabilities = { mode: 'account', requiresGroupType: true, requiresChannelType: false, groupTypes: protocolOptions }
const ownGroups = protocolOptions.map((option, i) => ({ id: String(i + 1), platform: option.id, groupName: option.name, multiplier: 1 }))
const mappingOptions = () => ({ ownGroups, mappings: [], connectionCapabilities: capabilities })
const rate = (type: string | null = 'openai, anthropic', platform = 'newapi', siteId = 'source') => ({ siteId, groupId: 'vip', groupName: 'VIP', siteName: 'Source', type, platform, currentMultiplier: 1 })
const pending = () => {
  let resolve!: (value: any) => void
  let reject!: (error: unknown) => void
  const promise = new Promise<any>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

// 编译实际 SFC 的 setup，而非复制表达式或仅用正则断言；网络与页面生命周期在测试边界替换。
const fixture = (overrides: Record<string, any> = {}) => {
  const api: Record<string, any> = {
    getMySiteMappingOptions: async () => mappingOptions(),
    listRealConnections: async () => [], listUpstreamKeys: async () => [], listAdminResources: async () => [],
    realConnect: async () => ({}), realBind: async () => ({}), realDisconnect: async () => {}, ...overrides,
  }
  const scope = vue.effectScope()
  const empty = () => vue.ref([])
  const groupRates = {
    rates: empty(), history: empty(), total: vue.ref(0), page: vue.ref(1), pageSize: vue.ref(20), totalPages: vue.ref(1), types: empty(), platforms: empty(),
    typeFilter: vue.ref(''), platformFilter: vue.ref(''), statusFilter: vue.ref('all'), sortMode: vue.ref('multiplierAsc'),
    statusCounts: vue.ref({ all: 0 }), serverSupportsStatusFilters: vue.ref(true), isLoading: vue.ref(false), isHistoryLoading: vue.ref(false), isActionLoading: vue.ref(false),
    errorKey: vue.ref(''), historyErrorKey: vue.ref(''), loadRates: async () => {}, loadHistory: async () => {}, saveType: async () => {},
    setSearch: async () => {}, setTypeFilter: async () => {}, setPlatformFilter: async () => {}, setStatusFilter: async () => {}, setSortMode: async () => {}, goToPage: async () => {},
  }
  Object.assign(globalThis, { document: { body: { style: { overflow: '' } }, activeElement: { focus() {} }, querySelector: () => null } })
  const component = evaluate(componentSource, {
    vue: { ...vue, onMounted: () => {}, onBeforeUnmount: () => {} },
    'vue-i18n': { useI18n: () => ({ t: (key: string) => key, locale: vue.ref('zh-CN') }) },
    'vue-router': { useRouter: () => ({}) }, 'lucide-vue-next': {}, '@/components/ui/button': { Button: {} },
    '../api/mySites': api, '../api/dashboardAdmin': { getDashboardAdminStatus: async () => ({ platform: 'sub2api' }) },
    '../composables/useGroupRates': { useGroupRates: () => groupRates }, '../types/mySites': types,
    '../utils/connectionOptions': { connectionErrorKey, effectiveConnectionProtocol, ...evaluate(read('../src/modules/admin/utils/connectionOptions.ts')) },
  }).default
  const state = scope.run(() => component.setup({}, { expose() {} }))
  return { api, state, stop: () => { state.closeConnector(); scope.stop() } }
}

test('New API 混合协议选择、切换清理及实际创建请求沿用同一 operationId', async () => {
  const calls: any[] = []
  const f = fixture({ realConnect: async (payload: any) => { calls.push(payload); throw new Error('secret remote response') } })
  try {
    await f.state.openConnector(rate())
    assert.equal(f.state.needsGroupTypeSelection.value, true)
    assert.equal(f.state.canSubmitConnect.value, false)
    f.state.selectedGroupType.value = 'anthropic'
    assert.deepEqual(f.state.filteredOwnGroups.value.map((g: any) => g.id), ['2'])
    f.state.toggleOwnGroup('2')
    assert.equal(f.state.canSubmitConnect.value, true)
    f.state.selectedGroupType.value = 'openai'
    assert.deepEqual(f.state.connectOwnGroups.value, [])
    f.state.toggleOwnGroup('1')
    await f.state.submitConnector()
    assert.equal(calls[0].groupType, 'openai')
    assert.deepEqual(calls[0].ownGroupIds, ['1'])
    assert.equal(f.state.realConnectError.value, 'admin.groupRates.connect.realFailed')
    await f.state.submitConnector()
    assert.equal(calls[1].operationId, calls[0].operationId)
    assert.notEqual(calls[0].operationId, '')
    assert.equal(f.state.connectingRate.value.siteId, 'source')
  } finally { f.stop() }
})

test('New API 单厂商可覆盖，清空或非法选值不偷偷使用默认协议', async () => {
  const f = fixture()
  try {
    await f.state.openConnector(rate('anthropic'))
    assert.equal(f.state.selectedGroupType.value, 'anthropic')
    assert.equal(f.state.needsGroupTypeSelection.value, true)
    f.state.selectedGroupType.value = 'openai'
    f.state.toggleOwnGroup('1')
    assert.equal(f.state.canSubmitConnect.value, true)
    for (const invalid of ['', 'unknown-protocol']) {
      f.state.selectedGroupType.value = invalid
      assert.equal(f.state.effectiveGroupType.value, '')
      assert.equal(f.state.canSubmitConnect.value, false)
    }
  } finally { f.stop() }
})

test('Sub2API 四协议保留自动识别和实际请求载荷', async () => {
  for (const [index, protocol] of protocolOptions.entries()) {
    let payload: any
    const f = fixture({ realConnect: async (value: any) => { payload = value; throw new Error('admin.upstream.errors.auth') } })
    try {
      await f.state.openConnector(rate(protocol.id, 'sub2api'))
      assert.equal(f.state.needsGroupTypeSelection.value, false)
      f.state.selectedGroupType.value = 'anthropic'
      assert.equal(f.state.effectiveGroupType.value, protocol.id)
      f.state.toggleOwnGroup(String(index + 1))
      await f.state.submitConnector()
      assert.equal(payload.groupType, protocol.id)
      assert.equal(f.state.realConnectError.value, 'admin.groupRates.connect.errors.remoteAuth')
    } finally { f.stop() }
  }
})

test('New API 管理端优先能力事实，建议不限制合法渠道', async () => {
  const options = { ...mappingOptions(), connectionCapabilities: { mode: 'channel', requiresGroupType: false, requiresChannelType: true, channelTypes: [{ id: 1, name: 'OpenAI' }, { id: 14, name: 'Anthropic' }, { id: 24, name: 'Gemini' }], suggestedChannelTypeByGroup: { openai: 1 } } }
  const f = fixture({ getMySiteMappingOptions: async () => options })
  try {
    await f.state.openConnector(rate('openai'))
    assert.equal(f.state.adminPlatform.value, '')
    assert.equal(f.state.isAdminNewAPI.value, true)
    assert.equal(f.state.selectedChannelType.value, 1)
    assert.deepEqual(f.state.filteredChannelTypes.value.map((option: any) => option.id), [1, 14, 24])
    assert.equal(f.state.filteredOwnGroups.value.length, 4)
    f.state.selectedChannelType.value = 14
    f.state.toggleOwnGroup('2')
    assert.equal(f.state.canSubmitConnect.value, true)
  } finally { f.stop() }
})

test('能力缺失或空协议列表明确阻断自动创建，但已有绑定不增加类型要求', async () => {
  for (const connectionCapabilities of [undefined, { ...capabilities, groupTypes: [] }]) {
    const f = fixture({ getMySiteMappingOptions: async () => ({ ...mappingOptions(), connectionCapabilities }) })
    try {
      await f.state.openConnector(rate())
      assert.equal(f.state.creationCapabilitiesError.value, 'admin.groupRates.connect.capabilitiesUnavailable')
      assert.equal(f.state.canSubmitConnect.value, false)
      await f.state.setConnectMode('bind')
      assert.equal(f.state.needsGroupTypeSelection.value, false)
      f.state.selectedKeyId.value = 'key-1'
      f.state.selectedAdminGroupId.value = '1'
      f.state.selectedAdminResourceId.value = 'account-1'
      assert.equal(f.state.canSubmitConnect.value, true)
    } finally { f.stop() }
  }
})

test('映射读取失败不冒充空列表，重载恢复且拒绝旧弹窗响应', async () => {
  const f = fixture({ getMySiteMappingOptions: async () => { throw new Error('secret-body') } })
  try {
    await f.state.openConnector(rate())
    assert.equal(f.state.mappingOptionsError.value, 'admin.groupRates.connect.mappingOptionsFailed')
    assert.equal(f.state.isLoadingMappingOptions.value, false)
    assert.equal(f.state.hasLoadedMappingOptions.value, false)
    f.api.getMySiteMappingOptions = async () => mappingOptions()
    await f.state.retryMappingOptions()
    assert.equal(f.state.mappingOptionsError.value, '')
    assert.equal(f.state.hasLoadedMappingOptions.value, true)
    const first = pending(), second = pending()
    f.state.hasLoadedMappingOptions.value = false
    f.api.getMySiteMappingOptions = () => first.promise
    const old = f.state.openConnector(rate(null, 'newapi', 'old'))
    f.api.getMySiteMappingOptions = () => second.promise
    const current = f.state.openConnector(rate('openai', 'newapi', 'current'))
    first.reject(new Error('stale secret'))
    await old
    assert.equal(f.state.mappingOptionsError.value, '')
    assert.equal(f.state.isLoadingMappingOptions.value, true)
    second.resolve(mappingOptions())
    await current
    assert.equal(f.state.selectedGroupType.value, 'openai')
    assert.equal(f.state.connectingRate.value.siteId, 'current')
  } finally { f.stop() }
})

test('凭据失败与空列表分离，可恢复且模式切换后旧响应失效', async () => {
  const f = fixture({ listUpstreamKeys: async () => { throw new Error('admin.upstream.errors.auth') } })
  try {
    await f.state.openConnector(rate())
    await f.state.setConnectMode('bind')
    assert.equal(f.state.keysError.value, 'admin.groupRates.connect.errors.remoteAuth')
    assert.equal(f.state.isLoadingKeys.value, false)
    f.api.listUpstreamKeys = async () => [{ id: 'key', name: 'Fixture' }]
    await f.state.loadUpstreamKeys(rate())
    assert.equal(f.state.keysError.value, '')
    assert.equal(f.state.upstreamKeys.value.length, 1)
    const delayed = pending()
    f.api.listUpstreamKeys = () => delayed.promise
    const loading = f.state.loadUpstreamKeys(rate())
    await f.state.setConnectMode('real')
    delayed.resolve([{ id: 'stale', name: 'Old' }])
    await loading
    assert.deepEqual(f.state.upstreamKeys.value, [])
    assert.equal(f.state.isLoadingKeys.value, false)
    assert.equal(f.state.keysError.value, '')
  } finally { f.stop() }
})

test('资源失败、目标切换与恢复；实际已有绑定不要求创建协议', async () => {
  let payload: any
  const f = fixture({ listUpstreamKeys: async () => [{ id: 'key', keyPreview: 'te...ey' }], listAdminResources: async () => { throw new Error('secret body') }, realBind: async (value: any) => { payload = value; throw new Error('secret') } })
  try {
    await f.state.openConnector(rate())
    await f.state.setConnectMode('bind')
    f.state.selectedKeyId.value = 'key'
    await f.state.loadAdminResourcesForGroup('1')
    assert.equal(f.state.adminResourcesError.value, 'admin.groupRates.connect.adminResourcesFailed')
    assert.equal(f.state.canSubmitConnect.value, false)
    const old = pending(), current = pending()
    f.api.listAdminResources = (id: string) => id === '1' ? old.promise : current.promise
    const a = f.state.loadAdminResourcesForGroup('1')
    const b = f.state.loadAdminResourcesForGroup('2')
    current.resolve([{ id: 'account', name: 'Account' }])
    await b
    old.reject(new Error('old secret'))
    await a
    assert.equal(f.state.adminResourcesError.value, '')
    assert.equal(f.state.adminResources.value[0].id, 'account')
    f.state.selectedAdminResourceId.value = 'account'
    assert.equal(f.state.canSubmitConnect.value, true)
    const operationId = f.state.connectOperationId.value
    await f.state.submitConnector()
    assert.equal(payload.groupType, '')
    assert.equal(payload.adminGroupId, '2')
    assert.equal(payload.adminResourceId, 'account')
    assert.equal(payload.operationId, operationId)
    assert.equal(f.state.realConnectError.value, 'admin.groupRates.connect.bindFailed')
    assert.equal(f.state.connectOperationId.value, operationId)
  } finally { f.stop() }
})

test('关闭弹窗后读取响应和错误均不得回填', async () => {
  const f = fixture()
  try {
    await f.state.openConnector(rate())
    await f.state.setConnectMode('bind')
    const key = pending(), resource = pending()
    f.api.listUpstreamKeys = () => key.promise
    f.api.listAdminResources = () => resource.promise
    const a = f.state.loadUpstreamKeys(rate()), b = f.state.loadAdminResourcesForGroup('1')
    f.state.closeConnector()
    key.reject(new Error('secret'))
    resource.resolve([{ id: 'old' }])
    await Promise.all([a, b])
    assert.equal(f.state.keysError.value, '')
    assert.deepEqual(f.state.adminResources.value, [])
    assert.equal(f.state.connectingRate.value, null)
    assert.equal(f.state.isLoadingKeys.value, false)
  } finally { f.stop() }
})

test('真实客户端的 502 不触发本站登出，固定错误可本地化而未知正文不泄露', async () => {
  let expired = 0
  const api = evaluate(read('../src/modules/admin/api/mySites.ts').replace("import.meta.env.VITE_API_BASE_URL", 'undefined'), {
    '@/modules/auth/api/auth': { authUnauthorizedErrorKey: 'auth.errors.unauthorized', getAccessToken: () => 'fixture', handleAuthExpired: () => { expired++ }, isUnauthorizedApiResponse: (status: number, payload: any) => status === 401 || payload.message === 'auth.errors.unauthorized' },
  })
  const oldFetch = globalThis.fetch
  try {
    for (const [message, expected] of [
      ['admin.upstream.errors.auth', 'admin.groupRates.connect.errors.remoteAuth'],
      ['secret-token-body', 'admin.groupRates.connect.keysFailed'],
      ['constructor', 'admin.groupRates.connect.keysFailed'],
    ]) {
      globalThis.fetch = async () => new Response(JSON.stringify({ message }), { status: 502 })
      await assert.rejects(() => api.listUpstreamKeys('site', 'group', 'name'), (error: unknown) => {
        assert.equal(connectionErrorKey(error, 'admin.groupRates.connect.keysFailed'), expected)
        return true
      })
    }
    assert.equal(expired, 0)
    globalThis.fetch = async () => new Response(JSON.stringify({ message: 'auth.errors.unauthorized' }), { status: 401 })
    await assert.rejects(() => api.getMySiteMappingOptions(), (error: unknown) => connectionErrorKey(error, '') === 'admin.groupRates.connect.errors.localAuth')
    assert.equal(expired, 1)
    globalThis.fetch = async () => { throw new Error('network secret') }
    await assert.rejects(() => api.listAdminResources('1'), (error: unknown) => connectionErrorKey(error, '') === 'admin.groupRates.connect.errors.network')
  } finally { globalThis.fetch = oldFetch }
})

test('所有对接错误投影均有中英本地化且未知键不直接送给翻译器', () => {
  const chinese = evaluate(read('../src/locales/zh-CN.ts')).default
  const english = evaluate(read('../src/locales/en-US.ts')).default
  const keys = ['localAuth', 'adminAuth', 'adminOnly', 'noCurrentAccount', 'connectionExists', 'groupTypeRequired', 'groupTypeInvalid', 'network', 'invalidUrl', 'remoteAuth', 'remoteRequest', 'invalidResponse', 'notFound']
  for (const dictionary of [chinese, english]) {
    for (const key of keys) assert.equal(typeof dictionary.admin.groupRates.connect.errors[key], 'string')
    for (const key of ['mappingOptionsFailed', 'mappingOptionsLoading', 'capabilitiesUnavailable', 'keysFailed', 'reload']) assert.equal(typeof dictionary.admin.groupRates.connect[key], 'string')
  }
  assert.equal(connectionErrorKey(new Error('admin.fake.untrusted'), 'fallback'), 'fallback')
  assert.equal(connectionErrorKey({ message: 'admin.upstream.errors.auth' }, 'fallback'), 'fallback')
})


test('创建与已有绑定成功均关闭弹窗并刷新，不额外重试写入', async () => {
  for (const mode of ['real', 'bind']) {
    let writes = 0, refreshes = 0
    const f = fixture({
      listUpstreamKeys: async () => [{ id: 'key' }],
      listAdminResources: async () => [{ id: 'account' }],
      realConnect: async () => { writes++; return {} },
      realBind: async () => { writes++; return {} },
      listRealConnections: async () => { refreshes++; return [] },
    })
    try {
      await f.state.openConnector(rate('openai'))
      if (mode === 'real') f.state.toggleOwnGroup('1')
      else {
        await f.state.setConnectMode('bind')
        f.state.selectedKeyId.value = 'key'
        await f.state.loadAdminResourcesForGroup('1')
        f.state.selectedAdminResourceId.value = 'account'
      }
      await f.state.submitConnector()
      assert.equal(writes, 1)
      assert.equal(refreshes, 1)
      assert.equal(f.state.connectingRate.value, null)
      assert.equal(f.state.isActionLoading.value, false)
      assert.equal(f.state.realConnectError.value, '')
    } finally { f.stop() }
  }
})


test('创建和绑定成功后映射读回失败保留写入成功并显示页面级刷新错误', async () => {
  for (const mode of ['real', 'bind']) {
    let writes = 0, reads = 0
    const f = fixture({
      getMySiteMappingOptions: async () => {
        reads++
        if (writes) throw new Error('remote-secret')
        return { ...mappingOptions(), mappings: [{ ownGroup: 'old', upstreamTargets: [] }] }
      },
      listUpstreamKeys: async () => [{ id: 'key' }], listAdminResources: async () => [{ id: 'account' }],
      realConnect: async () => { writes++; return {} }, realBind: async () => { writes++; return {} },
    })
    try {
      await f.state.openConnector(rate('openai'))
      assert.equal(f.state.mySiteMappings.value.length, 1)
      if (mode === 'real') f.state.toggleOwnGroup('1')
      else {
        await f.state.setConnectMode('bind')
        f.state.selectedKeyId.value = 'key'
        await f.state.loadAdminResourcesForGroup('1')
        f.state.selectedAdminResourceId.value = 'account'
      }
      await f.state.submitConnector()
      assert.equal(writes, 1)
      assert.equal(reads, 2)
      assert.equal(f.state.connectingRate.value, null)
      assert.equal(f.state.errorKey.value, 'admin.groupRates.errors.refreshFailed')
      assert.equal(f.state.realConnectError.value, '')
      assert.equal(f.state.isActionLoading.value, false)
      assert.equal(f.state.isLoadingMappingOptions.value, false)
      assert.equal(f.state.hasLoadedMappingOptions.value, false)
      assert.deepEqual(f.state.mySiteMappings.value, [])
    } finally { f.stop() }
  }
})
