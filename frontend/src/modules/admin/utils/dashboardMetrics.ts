import type { DashboardMetricsResponse, DashboardTrendPoint } from '../api/dashboardAdmin'
import type { DashboardColorToken, DashboardMetricData, DashboardMetricKey, TrendPoint } from '../types/dashboard'

const METRIC_CONFIGS: { key: DashboardMetricKey; color: DashboardColorToken }[] = [
  { key: 'todayProfit', color: 'primary' },
  { key: 'siteBalance', color: 'accent' },
  { key: 'todayPurchase', color: 'warning' },
  { key: 'netProfit', color: 'signal' },
  { key: 'upstreamBalance', color: 'primary' },
]

/** 预计续航取最近几个完整自然日（不含今天）的日均成本。 */
export const RUNWAY_RECENT_DAYS = 7

const dateFormatter = new Intl.DateTimeFormat('en-US', {
  timeZone: 'Asia/Shanghai',
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
})

// 与后端日快照使用同一业务日，不让浏览器本地时区改变周期边界。
export function dashboardDate(now = new Date()): string {
  const parts = dateFormatter.formatToParts(now)
  const part = (type: Intl.DateTimeFormatPartTypes) => parts.find(item => item.type === type)!.value
  return `${part('year')}-${part('month')}-${part('day')}`
}

// 成本有真实来源：已结算，或做过日内采样/部分日结。旧响应没有 purchaseRecorded 时按有记录处理。
// 只由营收补数建行、从未采样过成本的日子成本未知，不能当成 ¥0（否则净利润会等于营收）。
function purchaseKnown(point: DashboardTrendPoint): boolean {
  return point.purchaseComplete === true || point.purchaseRecorded !== false
}

// 历史成本采用现有记录；净利润 = 营收 − 成本，营收或成本任一未知时净利润也未知，图表和总额保持一致。
function historicalValue(point: DashboardTrendPoint | undefined, key: DashboardMetricKey): number | null {
  if (!point || !Number.isFinite(point[key])) return null
  switch (key) {
    case 'todayProfit':
      return point.revenueComplete === true ? point[key] : null
    case 'todayPurchase':
      return purchaseKnown(point) ? point[key] : null
    case 'netProfit':
      return point.revenueComplete === true && purchaseKnown(point) ? point[key] : null
    default:
      return point.balanceRecorded === true ? point[key] : null
  }
}

function dateLabel(date: string): string {
  const [, month, day] = date.split('-')
  return `${Number(month)}/${Number(day)}`
}

function shiftDate(date: string, days: number): string {
  const cursor = new Date(`${date}T00:00:00Z`)
  cursor.setUTCDate(cursor.getUTCDate() + days)
  return cursor.toISOString().slice(0, 10)
}

// 严格总额：缺一天即为 null。保留给需要“全周期完整”语义的调用方。
export function sumDashboardSeries(points: TrendPoint[]): number | null {
  if (points.length === 0 || points.some(point => point.value === null)) return null
  return points.reduce((total, point) => total + point.value!, 0)
}

export interface DashboardSeriesSummary {
  /** 已知日期的合计；全部未知时为 null。 */
  total: number | null
  /** 未计入合计的缺失天数。 */
  missingDays: number
}

// 周期总额只合计有数据的日子，并报告缺了几天；不再因为个别日缺数据就把整个周期总额隐藏。
export function summarizeDashboardSeries(points: TrendPoint[]): DashboardSeriesSummary {
  const known = points.filter(point => point.value !== null)
  return {
    total: known.length === 0 ? null : known.reduce((total, point) => total + point.value!, 0),
    missingDays: points.length - known.length,
  }
}

// 日均值只用有数据的日子；没有任何数据时为 null。
export function averageKnownValues(points: TrendPoint[]): number | null {
  const summary = summarizeDashboardSeries(points)
  const knownDays = points.length - summary.missingDays
  return summary.total === null || knownDays === 0 ? null : summary.total / knownDays
}

export function buildDashboardMetrics(
  live: DashboardMetricsResponse,
  trendPoints: DashboardTrendPoint[],
  now = new Date(),
): DashboardMetricData[] {
  // 优先使用服务器给出的北京日期，避免浏览器时钟偏差或请求跨零点时把实时值标到错误日期。
  const today = typeof live.date === 'string' && /^\d{4}-\d{2}-\d{2}$/.test(live.date) ? live.date : dashboardDate(now)
  // 用 UTC 做日期运算，日期本身已是北京时间；周一为自然周起点。
  const monday = new Date(`${today}T00:00:00Z`)
  monday.setUTCDate(monday.getUTCDate() - (monday.getUTCDay() + 6) % 7)
  const weekStart = monday.toISOString().slice(0, 10)
  const monthStart = `${today.slice(0, 7)}-01`
  const yesterday = shiftDate(today, -1)
  const recentDates = Array.from({ length: RUNWAY_RECENT_DAYS }, (_, index) => shiftDate(today, index - RUNWAY_RECENT_DAYS))

  const history = new Map(trendPoints.filter(point => point.date < today).map(point => [point.date, point]))
  // 所有指标共用完整自然日轴，缺日保留 null，避免图表错位或把缺口连成真实数据。
  const dates: string[] = []
  const cursor = new Date(`${weekStart < monthStart ? weekStart : monthStart}T00:00:00Z`)
  while (cursor.toISOString().slice(0, 10) <= today) {
    dates.push(cursor.toISOString().slice(0, 10))
    cursor.setUTCDate(cursor.getUTCDate() + 1)
  }
  // 实时站点余额读取失败时后端返回 balanceRecorded=false，此时余额未知而不是 ¥0。
  const liveKnown = (key: DashboardMetricKey) => key !== 'siteBalance' || live.balanceRecorded !== false

  return METRIC_CONFIGS.map(({ key, color }) => {
    const previous = historicalValue(history.get(yesterday), key)
    const comparison = previous === null ? [] : [{ label: dateLabel(yesterday), value: previous }]
    comparison.push({ label: dateLabel(today), value: live[key] })
    const series = (start: string): TrendPoint[] => dates.filter(date => date >= start).map(date => ({
      label: dateLabel(date),
      value: date === today ? live[key] : historicalValue(history.get(date), key),
    }))
    return {
      key,
      color,
      current: live[key],
      currentKnown: liveKnown(key),
      comparison,
      series: { week: series(weekStart), month: series(monthStart) },
      recent: recentDates.map(date => ({ label: dateLabel(date), value: historicalValue(history.get(date), key) })),
    }
  })
}
