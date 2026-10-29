import assert from 'node:assert/strict'
import { test } from 'node:test'
import { readFileSync } from 'node:fs'
import { runInNewContext } from 'node:vm'
import ts from 'typescript'
import { computed, ref } from 'vue'
import { averageKnownValues, buildDashboardMetrics, dashboardDate, summarizeDashboardSeries, sumDashboardSeries } from '../src/modules/admin/utils/dashboardMetrics.ts'
import { computeDelta, formatCny } from '../src/modules/admin/utils/dashboard.ts'
import zh from '../src/locales/zh-CN.ts'
import en from '../src/locales/en-US.ts'

const values = (revenue: number) => ({
  revenueComplete: true,
  purchaseComplete: true,
  balanceRecorded: true,
  todayProfit: revenue,
  todayPurchase: revenue / 4,
  netProfit: revenue * 3 / 4,
  siteBalance: 200,
  upstreamBalance: 300,
  groupCount: 2,
})

const addDays = (date: string, days: number) => {
  const result = new Date(`${date}T00:00:00Z`)
  result.setUTCDate(result.getUTCDate() + days)
  return result.toISOString().slice(0, 10)
}

const history = (today: string, days = 35) => Array.from({ length: days }, (_, i) => ({
  date: addDays(today, i - days),
  ...values(i + 1),
}))

// 各案例分别约束自然周、自然月，不允许按记录条数截断。
for (const [today, weekStart, weekDays, monthDays] of [
  ['2026-09-07', '9/7', 1, 7],
  ['2026-09-09', '9/7', 3, 9],
  ['2026-09-13', '9/7', 7, 13],
  ['2026-09-01', '8/31', 2, 1],
  ['2026-09-30', '9/28', 3, 30],
  ['2026-08-31', '8/31', 1, 31],
  ['2026-02-28', '2/23', 6, 28],
  ['2028-02-29', '2/28', 2, 29],
  ['2027-01-01', '12/28', 5, 1],
] as const) {
  test(`${today} 按北京时间自然周和自然月统计`, () => {
    const metrics = buildDashboardMetrics(values(100), history(today), new Date(`${today}T12:00:00+08:00`))
    for (const metric of metrics) {
      assert.equal(metric.series.week.length, weekDays)
      assert.equal(metric.series.month.length, monthDays)
      assert.equal(metric.series.week[0].label, weekStart)
      assert.equal(metric.series.month[0].label, `${Number(today.slice(5, 7))}/1`)
      assert.equal(metric.current, values(100)[metric.key])
      assert.equal(metric.series.month.at(-1)?.value, metric.current)
    }
  })
}

test('31 天的月份完整保留月初，30 条历史快照加上今日共 31 天', () => {
  const metric = buildDashboardMetrics(values(1), history('2026-08-31', 30), new Date('2026-08-31T04:00:00Z'))[0]
  assert.equal(metric.series.month.length, 31)
  assert.equal(metric.series.month[0].label, '8/1')
})

test('周期总额与图表共用筛选结果，旧周期的值不计入', () => {
  const metrics = buildDashboardMetrics(values(100), [
    ...Array.from({ length: 8 }, (_, day) => ({ date: addDays('2026-09-01', day), ...values(0) })),
    { date: '2026-08-31', ...values(10000) },
    { date: '2026-09-01', ...values(10) },
    { date: '2026-09-06', ...values(20) },
    { date: '2026-09-07', ...values(30) },
    { date: '2026-09-08', ...values(40) },
  ], new Date('2026-09-09T04:00:00Z'))
  for (const key of ['todayProfit', 'todayPurchase', 'netProfit'] as const) {
    const metric = metrics.find(item => item.key === key)!
    assert.equal(sumDashboardSeries(metric.series.week), values(170)[key])
    assert.equal(sumDashboardSeries(metric.series.month), values(200)[key])
  }
})

test('月初的日对比保留昨日，不因本月只有一个点而归零', () => {
  const metrics = buildDashboardMetrics(values(100), history('2026-09-01'), new Date('2026-09-01T04:00:00Z'))
  const revenue = metrics.find(metric => metric.key === 'todayProfit')!
  assert.deepEqual(revenue.comparison, [{ label: '8/31', value: 35 }, { label: '9/1', value: 100 }])
  assert.deepEqual(computeDelta(revenue.comparison.map(point => point.value)), { amount: 65, direction: 'up' })
  const profit = metrics.find(metric => metric.key === 'netProfit')!
  const margins = revenue.comparison.map((point, index) => profit.comparison[index].value / point.value * 100)
  assert.deepEqual(margins, [75, 75])
})

test('空历史保留自然日缺口，缺失日期为未知而非零或上周快照', () => {
  const now = new Date('2026-09-09T04:00:00Z')
  const empty = buildDashboardMetrics(values(0), [], now)[0]
  assert.deepEqual(empty.series.week, [{ label: '9/7', value: null }, { label: '9/8', value: null }, { label: '9/9', value: 0 }])
  assert.equal(empty.series.month.length, 9)
  assert.ok(empty.series.month.slice(0, -1).every(point => point.value === null))
  assert.equal(sumDashboardSeries(empty.series.week), null)
  const sparse = buildDashboardMetrics(values(100), [
    { date: '2026-09-08', ...values(40) },
    { date: '2026-08-31', ...values(1000) },
    { date: '2026-09-01', ...values(10) },
  ], now)[0]
  assert.deepEqual(sparse.series.week, [{ label: '9/7', value: null }, { label: '9/8', value: 40 }, { label: '9/9', value: 100 }])
  assert.deepEqual(sparse.series.month.filter(point => point.value !== null).map(point => point.label), ['9/1', '9/8', '9/9'])
  assert.equal(sumDashboardSeries(sparse.series.month), null)
})

test('今日快照和未来日期不重复计入，也不修改接口原数据', () => {
  const points = Object.freeze([
    Object.freeze({ date: '2026-09-10', ...values(10000) }),
    Object.freeze({ date: '2026-09-09', ...values(9000) }),
    Object.freeze({ date: '2026-09-08', ...values(40) }),
  ])
  const metric = buildDashboardMetrics(values(100), [...points], new Date('2026-09-09T04:00:00Z'))[0]
  assert.deepEqual(metric.series.week, [{ label: '9/7', value: null }, { label: '9/8', value: 40 }, { label: '9/9', value: 100 }])
  assert.equal(points[0].date, '2026-09-10')
})

test('北京时间零点切换自然周期，浏览器在其它时区也不偏移标签', () => {
  const originalTZ = process.env.TZ
  try {
    for (const timezone of ['Etc/GMT+12', 'Pacific/Kiritimati', 'America/Los_Angeles']) {
      process.env.TZ = timezone
      assert.equal(dashboardDate(new Date('2026-08-31T15:59:59Z')), '2026-08-31')
      assert.equal(dashboardDate(new Date('2026-08-31T16:00:00Z')), '2026-09-01')
      const metric = buildDashboardMetrics(values(100), history('2026-09-01'), new Date('2026-08-31T16:00:00Z'))[0]
      assert.deepEqual(metric.series.month, [{ label: '9/1', value: 100 }])
      assert.deepEqual(metric.series.week.map(point => point.label), ['8/31', '9/1'])
    }
  } finally {
    if (originalTZ === undefined) delete process.env.TZ
    else process.env.TZ = originalTZ
  }
})

test('中英文按钮明确显示当前自然周期', () => {
  assert.equal(zh.admin.dashboard.period.week, '本周')
  assert.equal(zh.admin.dashboard.period.month, '本月')
  assert.equal(en.admin.dashboard.period.week, 'This Week')
  assert.equal(en.admin.dashboard.period.month, 'This Month')
})

// 昨日缺失与昨日真实零值是不同事实，禁止用前天或占位零值补齐比较。
test('缺昨日时日对比只含今日，不使用前天或其它更早快照', () => {
  const metrics = buildDashboardMetrics(values(100), [
    { date: '2026-09-07', ...values(40) },
    { date: '2026-09-01', ...values(20) },
  ], new Date('2026-09-09T04:00:00Z'))
  for (const metric of metrics) {
    assert.deepEqual(metric.comparison, [{ label: '9/9', value: values(100)[metric.key] }])
    assert.deepEqual(metric.series.week.map(point => point.label), ['9/7', '9/8', '9/9'])
    assert.equal(metric.series.week[1].value, null)
  }
})

test('空历史没有昨日比较，昨日真实零值仍参与比较', () => {
  const now = new Date('2026-09-09T04:00:00Z')
  const empty = buildDashboardMetrics(values(100), [], now)[0]
  assert.deepEqual(empty.comparison, [{ label: '9/9', value: 100 }])
  const zero = buildDashboardMetrics(values(100), [{ date: '2026-09-08', ...values(0) }], now)[0]
  assert.deepEqual(zero.comparison, [{ label: '9/8', value: 0 }, { label: '9/9', value: 100 }])
  assert.deepEqual(computeDelta(zero.comparison.map(point => point.value)), { amount: 100, direction: 'up' })
})

test('跨年北京时间零点精确保留昨日，不受浏览器时区影响', () => {
  const metrics = buildDashboardMetrics(values(100), [
    { date: '2026-12-30', ...values(999) },
    { date: '2026-12-31', ...values(25) },
  ], new Date('2026-12-31T16:00:00Z'))
  assert.deepEqual(metrics[0].comparison, [{ label: '12/31', value: 25 }, { label: '1/1', value: 100 }])
  assert.deepEqual(metrics[0].series.month, [{ label: '1/1', value: 100 }])
})

test('中英文缺昨日文案不将缺失表达为零差额', () => {
  assert.equal(zh.admin.dashboard.delta.missingYesterday, '缺少昨日数据')
  assert.equal(en.admin.dashboard.delta.missingYesterday, 'Yesterday’s data unavailable')
})

// 成本采用既有记录；净利润 = 营收 − 成本，营收未确认时净利同为未知；余额仍保留原完整性门禁。
test('成本采用现有记录，营收未确认时净利未知，余额门禁不变，日期轴始终对齐', () => {
  const metrics = buildDashboardMetrics(values(100), [
    { date: '2026-09-07', ...values(20), revenueComplete: false, balanceRecorded: false },
    { date: '2026-09-08', ...values(40), purchaseComplete: false, balanceRecorded: false },
  ], new Date('2026-09-09T04:00:00Z'))
  const revenue = metrics.find(item => item.key === 'todayProfit')!
  const cost = metrics.find(item => item.key === 'todayPurchase')!
  const profit = metrics.find(item => item.key === 'netProfit')!
  assert.deepEqual(revenue.series.week.map(point => point.value), [null, 40, 100])
  assert.deepEqual(cost.series.week.map(point => point.value), [5, 10, 25])
  assert.deepEqual(profit.series.week.map(point => point.value), [null, 30, 75])
  assert.equal(revenue.comparison.length, 2)
  assert.equal(cost.comparison.length, 2)
  assert.equal(profit.comparison.length, 2)
  for (const metric of metrics) {
    assert.deepEqual(metric.series.week.map(point => point.label), ['9/7', '9/8', '9/9'])
    const expected = metric.key === 'todayPurchase' ? 40 : null
    assert.equal(sumDashboardSeries(metric.series.week), expected)
  }
  assert.deepEqual(summarizeDashboardSeries(profit.series.week), { total: 105, missingDays: 1 })
  for (const key of ['siteBalance', 'upstreamBalance']) {
    const metric = metrics.find(item => item.key === key)!
    assert.ok(metric.series.week.slice(0, -1).every(point => point.value === null))
    assert.equal(metric.comparison.length, 1)
  }
})

test('旧响应缺标志或非布尔真值仍展示成本，不放宽营收、净利与余额', () => {
  for (const complete of [undefined, false, 1, 'true']) {
    const point = { date: '2026-09-08', ...values(40), revenueComplete: complete, purchaseComplete: complete, balanceRecorded: complete }
    // 模拟运行时旧缓存或不符合接口声明的响应。
    const metrics = buildDashboardMetrics(values(100), [point as never], new Date('2026-09-09T04:00:00Z'))
    for (const metric of metrics) {
      const adopted = metric.key === 'todayPurchase'
      assert.equal(metric.comparison.length, adopted ? 2 : 1)
      assert.equal(metric.series.week[1].value, adopted ? values(40)[metric.key] : null)
      assert.equal(sumDashboardSeries(metric.series.week), null)
    }
  }
})

test('周期全日完整才有总额，已确认零值保留为零，缺行与非有限金额保持未知', () => {
  const metrics = buildDashboardMetrics(values(0), [
    { date: '2026-09-07', ...values(0) },
    { date: '2026-09-08', ...values(0) },
  ], new Date('2026-09-09T04:00:00Z'))
  for (const key of ['todayProfit', 'todayPurchase', 'netProfit']) {
    const metric = metrics.find(item => item.key === key)!
    assert.equal(sumDashboardSeries(metric.series.week), 0)
    assert.equal(sumDashboardSeries(metric.series.month), null)
  }
  const invalid = buildDashboardMetrics(values(0), [{ date: '2026-09-08', ...values(Number.NaN) }], new Date('2026-09-09T04:00:00Z'))[0]
  assert.equal(invalid.series.week[1].value, null)
  assert.equal(invalid.comparison.length, 1)
  assert.equal(sumDashboardSeries([]), null)
  assert.equal(zh.admin.dashboard.performance.incomplete, '数据不完整')
  assert.equal(en.admin.dashboard.performance.incomplete, 'Incomplete data')
})

// 运行页面现有计算代码，验证真实消费者，而非在测试里另写卡片和图表规则。
test('页面卡片、图表、总额、利润率与续航采用现有成本，缺口单独提示且不清空整个周期', () => {
  const source = readFileSync(new URL('../src/modules/admin/views/DashboardView.vue', import.meta.url), 'utf8')
  const start = source.indexOf('const METRIC_META:')
  const end = source.indexOf('const sortedGroups =')
  const capitalStart = source.indexOf('const siteBalance =')
  const capitalEnd = source.indexOf('const upstreamIssueCount =')
  assert.ok(start >= 0 && end > start && capitalStart >= end && capitalEnd > capitalStart)
  const metrics = ref(buildDashboardMetrics(values(100), [
    { date: '2026-09-07', ...values(20) },
    { date: '2026-09-08', ...values(40), purchaseComplete: false, netProfit: -17 },
  ], new Date('2026-09-09T04:00:00Z')))
  const context = {
    computed, ref, metrics, locale: ref('zh-CN'), computeDelta, formatCny, summarizeDashboardSeries, averageKnownValues,
    t: (key: string) => key,
    TrendingUp: {}, Wallet: {}, ShoppingCart: {}, PiggyBank: {}, Landmark: {}, Gauge: {},
    chartTheme: ref({ muted: '#777', card: '#fff', border: '#ccc', foreground: '#111', primary: '#00f', warning: '#fa0', signal: '#0a0' }),
  }
  const script = source.slice(start, end) + source.slice(capitalStart, capitalEnd)
    + '\n;({ cards, period, periodTotals, periodSummaries, performanceChartOption, runwayDays, marginSeries, siteBalance, coverageRatio })'
  const result = runInNewContext(ts.transpile(script, { target: ts.ScriptTarget.ES2022 }), context)
  assert.equal(result.cards.value.length, 4)
  assert.equal(result.cards.value[0].deltaCaption, 'admin.dashboard.delta.vsPrev')
  for (const card of result.cards.value) {
    assert.equal(card.deltaCaption, 'admin.dashboard.delta.vsPrev')
    assert.notEqual(card.deltaText, '')
  }
  assert.equal(result.cards.value[2].deltaText, '¥92.00')
  assert.deepEqual(Array.from(result.marginSeries.value), [-42.5, 75])
  assert.equal(result.periodTotals.value.revenue, 160)
  assert.equal(result.periodTotals.value.cost, 40)
  assert.equal(result.periodTotals.value.profit, 73)
  // 续航只看最近 7 个完整日（不含今天的部分值）：9/7 成本 5、9/8 成本 10，日均 7.5。
  assert.equal(result.runwayDays.value, 40)
  const chart = result.performanceChartOption.value
  assert.deepEqual(Array.from(chart.xAxis.data), ['9/7', '9/8', '9/9'])
  assert.deepEqual(Array.from(chart.series[0].data), [20, 40, 100])
  assert.deepEqual(Array.from(chart.series[1].data), [5, 10, 25])
  assert.deepEqual(Array.from(chart.series[2].data), [15, -17, 75])
  assert.equal(chart.series[2].connectNulls, false)
  result.period.value = 'month'
  // 月初几天没有记录时只合计有数据的日子，并报告缺失天数，不再把整月总额隐藏。
  assert.equal(result.periodTotals.value.revenue, 160)
  assert.equal(result.periodSummaries.value.revenue.missingDays, 6)
  assert.equal(result.performanceChartOption.value.xAxis.data.length, 9)

  result.period.value = 'week'
  metrics.value = buildDashboardMetrics(values(100), [
    { date: '2026-09-07', ...values(20) },
    { date: '2026-09-08', ...values(0) },
  ], new Date('2026-09-09T04:00:00Z'))
  for (const card of result.cards.value.slice(0, 3)) {
    assert.equal(card.deltaCaption, 'admin.dashboard.delta.vsPrev')
    assert.notEqual(card.deltaText, '')
  }
  // 昨日营收为 0 时昨日利润率无意义，不做利润率对比。
  assert.equal(result.cards.value[3].deltaText, '')
  assert.equal(result.periodTotals.value.revenue, 120)
  assert.equal(result.periodTotals.value.cost, 30)
  assert.equal(result.periodTotals.value.profit, 90)
  assert.equal(result.runwayDays.value, 120)

  // 整月旧标志不再隐藏现有成本和净利，页面仍直接消费存储负净利。
  metrics.value = buildDashboardMetrics(values(100), Array.from({ length: 29 }, (_, day) => ({
    date: addDays('2026-09-01', day), ...values(20), purchaseComplete: false, todayPurchase: 2, netProfit: -3,
  })), new Date('2026-09-30T04:00:00Z'))
  result.period.value = 'month'
  assert.equal(result.periodTotals.value.cost, 83)
  assert.equal(result.periodTotals.value.profit, -12)
  assert.equal(result.runwayDays.value, 150)
  assert.equal(result.performanceChartOption.value.series[1].data.length, 30)
  assert.ok(result.performanceChartOption.value.series[1].data.every((value: number | null) => value !== null))
  assert.equal(result.cards.value[1].deltaText, '¥23.00')
  assert.equal(result.cards.value[2].deltaText, '¥78.00')
  assert.deepEqual(Array.from(result.marginSeries.value), [-15, 75])

  // 保留真实缺行/非有限值的门禁，不把恢复展示变成填零或跳过缺口。
  for (const yesterday of [[], [{ date: '2026-09-08', ...values(40), todayPurchase: NaN, netProfit: Infinity }]]) {
    result.period.value = 'week'
    metrics.value = buildDashboardMetrics(values(100), [{ date: '2026-09-07', ...values(20) }, ...yesterday], new Date('2026-09-09T04:00:00Z'))
    for (const card of result.cards.value.slice(1)) {
      assert.equal(card.deltaCaption, 'admin.dashboard.delta.missingYesterday')
      assert.equal(card.deltaText, '')
    }
    assert.equal(result.periodTotals.value.cost, 30)
    assert.equal(result.periodSummaries.value.cost.missingDays, 1)
    assert.equal(result.periodTotals.value.profit, 90)
    assert.equal(result.runwayDays.value, 60)
    assert.deepEqual(Array.from(result.performanceChartOption.value.xAxis.data), ['9/7', '9/8', '9/9'])
    assert.deepEqual(Array.from(result.performanceChartOption.value.series[1].data), [5, null, 25])
    assert.deepEqual(Array.from(result.performanceChartOption.value.series[2].data), [15, null, 75])
  }

  // 营收为 0（刚过零点）时利润率显示“暂无”，不显示 0% 和虚假的百分点差。
  metrics.value = buildDashboardMetrics({ ...values(0), todayPurchase: 3, netProfit: -3 }, [
    { date: '2026-09-08', ...values(40) },
  ], new Date('2026-09-09T04:00:00Z'))
  const margin = result.cards.value[3]
  assert.equal(margin.value, 'admin.dashboard.common.unavailable')
  assert.equal(margin.deltaText, '')
  assert.equal(margin.deltaCaption, 'admin.dashboard.delta.missingYesterday')

  // 实时站点余额读取失败时显示未知，覆盖率不可用，而不是按 ¥0 计算。
  metrics.value = buildDashboardMetrics({ ...values(100), balanceRecorded: false }, [], new Date('2026-09-09T04:00:00Z'))
  assert.equal(result.siteBalance.value, null)
  assert.equal(result.coverageRatio.value, null)
})

// 展示采用记录不是重算账目；收入未完成不屏蔽成本，但净利润依赖营收，同为未知。
test('整月收入成本标志均false时保留成本原值，净利随营收未知，不改输入', () => {
  const points = Array.from({ length: 29 }, (_, day) => Object.freeze({
    date: addDays('2026-09-01', day), ...values(20), revenueComplete: false, purchaseComplete: false,
    todayPurchase: 2, netProfit: -3,
  }))
  const original = JSON.stringify(points)
  const metrics = buildDashboardMetrics(values(100), points, new Date('2026-09-30T04:00:00Z'))
  const cost = metrics.find(metric => metric.key === 'todayPurchase')!
  const profit = metrics.find(metric => metric.key === 'netProfit')!
  assert.deepEqual(cost.series.month.map(point => point.value), [...Array(29).fill(2), 25])
  assert.deepEqual(profit.series.month.map(point => point.value), [...Array(29).fill(null), 75])
  assert.equal(sumDashboardSeries(cost.series.month), 83)
  assert.equal(sumDashboardSeries(profit.series.month), null)
  assert.equal(sumDashboardSeries(cost.series.week), 29)
  assert.deepEqual(summarizeDashboardSeries(profit.series.week), { total: 75, missingDays: 2 })
  assert.deepEqual(cost.comparison, [{ label: '9/29', value: 2 }, { label: '9/30', value: 25 }])
  assert.deepEqual(profit.comparison, [{ label: '9/30', value: 75 }])
  assert.equal(metrics.find(metric => metric.key === 'todayProfit')!.series.month[0].value, null)
  assert.equal(JSON.stringify(points), original)
})

test('旧响应缺成本标志时成本零值和存储负净利均保留，各项非有限值独立成为缺口', () => {
  const now = new Date('2026-09-08T04:00:00Z')
  const { purchaseComplete, ...legacy } = values(100)
  const point = { ...legacy, date: '2026-09-07', todayPurchase: 0, netProfit: -7 }
  const metrics = buildDashboardMetrics(values(0), [point as never], now)
  const cost = metrics.find(metric => metric.key === 'todayPurchase')!
  const profit = metrics.find(metric => metric.key === 'netProfit')!
  assert.deepEqual(cost.comparison.map(point => point.value), [0, 0])
  assert.deepEqual(profit.comparison.map(point => point.value), [-7, 0])
  assert.equal(sumDashboardSeries(cost.series.week), 0)
  assert.equal(sumDashboardSeries(profit.series.week), -7)
  for (const key of ['todayPurchase', 'netProfit'] as const) {
    for (const value of [NaN, Infinity, -Infinity]) {
      const invalid = buildDashboardMetrics(values(0), [{ ...point, [key]: value } as never], now)
      const metric = invalid.find(metric => metric.key === key)!
      assert.equal(metric.series.week[0].value, null)
      assert.equal(metric.comparison.length, 1)
      assert.equal(sumDashboardSeries(metric.series.week), null)
      const other = invalid.find(metric => metric.key === (key === 'todayPurchase' ? 'netProfit' : 'todayPurchase'))!
      assert.equal(other.series.week[0].value, key === 'todayPurchase' ? -7 : 0)
    }
  }
})

// 只由营收补数建行、从未采样过成本的日子：成本未知，不能显示成 ¥0 让净利润等于营收。
test('成本未记录的日子显示为缺口，不当成 0；周期总额只合计有数据的日子', () => {
  const metrics = buildDashboardMetrics(values(100), [
    { date: '2026-09-07', ...values(20) },
    { date: '2026-09-08', ...values(40), purchaseComplete: false, purchaseRecorded: false, todayPurchase: 0, netProfit: 40 },
  ], new Date('2026-09-09T04:00:00Z'))
  const revenue = metrics.find(item => item.key === 'todayProfit')!
  const cost = metrics.find(item => item.key === 'todayPurchase')!
  const profit = metrics.find(item => item.key === 'netProfit')!
  assert.deepEqual(revenue.series.week.map(point => point.value), [20, 40, 100])
  assert.deepEqual(cost.series.week.map(point => point.value), [5, null, 25])
  assert.deepEqual(profit.series.week.map(point => point.value), [15, null, 75])
  assert.equal(cost.comparison.length, 1)
  assert.equal(profit.comparison.length, 1)
  assert.deepEqual(summarizeDashboardSeries(cost.series.week), { total: 30, missingDays: 1 })
  assert.equal(averageKnownValues(cost.recent), 5)
  // 已采样（purchaseRecorded=true）或已结算的成本照常展示。
  const sampled = buildDashboardMetrics(values(100), [
    { date: '2026-09-08', ...values(40), purchaseComplete: false, purchaseRecorded: true },
  ], new Date('2026-09-09T04:00:00Z')).find(item => item.key === 'todayPurchase')!
  assert.equal(sampled.comparison.length, 2)
  assert.deepEqual(summarizeDashboardSeries([]), { total: null, missingDays: 0 })
  assert.equal(averageKnownValues([{ label: '9/8', value: null }]), null)
})

test('以服务器北京日期为今天，不受浏览器时钟偏差影响', () => {
  // 浏览器时钟仍在 9/8，但服务器已是 9/9：实时值必须落在 9/9，昨日为 9/8。
  const metrics = buildDashboardMetrics({ ...values(100), date: '2026-09-09' }, [
    { date: '2026-09-08', ...values(40) },
  ], new Date('2026-09-08T15:59:00Z'))
  const revenue = metrics.find(item => item.key === 'todayProfit')!
  assert.deepEqual(revenue.comparison, [{ label: '9/8', value: 40 }, { label: '9/9', value: 100 }])
})

test('金额负数格式与浮点误差', () => {
  assert.equal(formatCny(-17), '-¥17.00')
  assert.equal(formatCny(-0.001), '¥0.00')
  assert.equal(formatCny(1234.5), '¥1,234.50')
  assert.deepEqual(computeDelta([0.3, 0.1 + 0.2]), { amount: 0, direction: 'flat' })
  assert.deepEqual(computeDelta([10, 7.5]), { amount: -2.5, direction: 'down' })
})
