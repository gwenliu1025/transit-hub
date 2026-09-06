import assert from 'node:assert/strict'
import { test } from 'node:test'
import { buildDashboardMetrics, dashboardDate } from '../src/modules/admin/utils/dashboardMetrics.ts'
import { computeDelta } from '../src/modules/admin/utils/dashboard.ts'
import zh from '../src/locales/zh-CN.ts'
import en from '../src/locales/en-US.ts'

const values = (revenue: number) => ({
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
    { date: '2026-08-31', ...values(10000) },
    { date: '2026-09-01', ...values(10) },
    { date: '2026-09-06', ...values(20) },
    { date: '2026-09-07', ...values(30) },
    { date: '2026-09-08', ...values(40) },
  ], new Date('2026-09-09T04:00:00Z'))
  for (const key of ['todayProfit', 'todayPurchase', 'netProfit'] as const) {
    const metric = metrics.find(item => item.key === key)!
    assert.equal(metric.series.week.reduce((sum, point) => sum + point.value, 0), values(170)[key])
    assert.equal(metric.series.month.reduce((sum, point) => sum + point.value, 0), values(200)[key])
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

test('空历史只展示今日，缺失的日期不补零或从上周凑足七条', () => {
  const now = new Date('2026-09-09T04:00:00Z')
  const empty = buildDashboardMetrics(values(0), [], now)[0]
  assert.deepEqual(empty.series.week, [{ label: '9/9', value: 0 }])
  assert.deepEqual(empty.series.month, empty.series.week)
  const sparse = buildDashboardMetrics(values(100), [
    { date: '2026-09-08', ...values(40) },
    { date: '2026-08-31', ...values(1000) },
    { date: '2026-09-01', ...values(10) },
  ], now)[0]
  assert.deepEqual(sparse.series.week.map(point => point.label), ['9/8', '9/9'])
  assert.deepEqual(sparse.series.month.map(point => point.label), ['9/1', '9/8', '9/9'])
})

test('今日快照和未来日期不重复计入，也不修改接口原数据', () => {
  const points = Object.freeze([
    Object.freeze({ date: '2026-09-10', ...values(10000) }),
    Object.freeze({ date: '2026-09-09', ...values(9000) }),
    Object.freeze({ date: '2026-09-08', ...values(40) }),
  ])
  const metric = buildDashboardMetrics(values(100), [...points], new Date('2026-09-09T04:00:00Z'))[0]
  assert.deepEqual(metric.series.week, [{ label: '9/8', value: 40 }, { label: '9/9', value: 100 }])
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
