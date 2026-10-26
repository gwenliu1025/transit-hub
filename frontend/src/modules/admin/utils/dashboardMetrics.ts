import type { DashboardMetricsResponse, DashboardTrendPoint } from '../api/dashboardAdmin'
import type { DashboardColorToken, DashboardMetricData, DashboardMetricKey, TrendPoint } from '../types/dashboard'

const METRIC_CONFIGS: { key: DashboardMetricKey; color: DashboardColorToken }[] = [
  { key: 'todayProfit', color: 'primary' },
  { key: 'siteBalance', color: 'accent' },
  { key: 'todayPurchase', color: 'warning' },
  { key: 'netProfit', color: 'signal' },
  { key: 'upstreamBalance', color: 'primary' },
]

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

// 历史成本与净利按用户采用的现有记录展示；日结标志仍归后端写入保护，不作这两项的展示门禁。
function historicalValue(point: DashboardTrendPoint | undefined, key: DashboardMetricKey): number | null {
  if (!point || !Number.isFinite(point[key])) return null
  // 直接取存储净利，不按当前收入、成本或倍率重算历史账。
  if (key === 'todayPurchase' || key === 'netProfit') return point[key]
  const complete = key === 'todayProfit' ? point.revenueComplete === true : point.balanceRecorded === true
  return complete ? point[key] : null
}

function dateLabel(date: string): string {
  const [, month, day] = date.split('-')
  return `${Number(month)}/${Number(day)}`
}

// 缺一天即不是完整周期总额，避免把已确认小计冒充总额。
export function sumDashboardSeries(points: TrendPoint[]): number | null {
  if (points.length === 0 || points.some(point => point.value === null)) return null
  return points.reduce((total, point) => total + point.value!, 0)
}

export function buildDashboardMetrics(
  live: DashboardMetricsResponse,
  trendPoints: DashboardTrendPoint[],
  now = new Date(),
): DashboardMetricData[] {
  const today = dashboardDate(now)
  // 用 UTC 做日期运算，日期本身已是北京时间；周一为自然周起点。
  const monday = new Date(`${today}T00:00:00Z`)
  monday.setUTCDate(monday.getUTCDate() - (monday.getUTCDay() + 6) % 7)
  const weekStart = monday.toISOString().slice(0, 10)
  const monthStart = `${today.slice(0, 7)}-01`
  const previousDay = new Date(`${today}T00:00:00Z`)
  previousDay.setUTCDate(previousDay.getUTCDate() - 1)
  const yesterday = previousDay.toISOString().slice(0, 10)

  const history = new Map(trendPoints.filter(point => point.date < today).map(point => [point.date, point]))
  // 所有指标共用完整自然日轴，缺日保留 null，避免图表错位或把缺口连成真实数据。
  const dates: string[] = []
  const cursor = new Date(`${weekStart < monthStart ? weekStart : monthStart}T00:00:00Z`)
  while (cursor.toISOString().slice(0, 10) <= today) {
    dates.push(cursor.toISOString().slice(0, 10))
    cursor.setUTCDate(cursor.getUTCDate() + 1)
  }

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
      comparison,
      series: { week: series(weekStart), month: series(monthStart) },
    }
  })
}
