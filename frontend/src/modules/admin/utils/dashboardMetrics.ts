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

function toSeries(points: DashboardTrendPoint[], key: DashboardMetricKey): TrendPoint[] {
  return points.map(point => {
    const [, month, day] = point.date.split('-')
    return { label: `${Number(month)}/${Number(day)}`, value: point[key] }
  })
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

  // 历史只消费今天之前的快照，今天仅追加一次实时值；缺失日期不虚构为零。
  const points = trendPoints.filter(point => point.date < today).sort((a, b) => a.date.localeCompare(b.date))
  points.push({ ...live, date: today })
  const week = points.filter(point => point.date >= weekStart)
  const month = points.filter(point => point.date >= monthStart)

  return METRIC_CONFIGS.map(({ key, color }) => ({
    key,
    color,
    current: live[key],
    // 日对比继续使用最后两个有效点，月初也保留上月末的数据。
    comparison: toSeries(points.slice(-2), key),
    series: { week: toSeries(week, key), month: toSeries(month, key) },
  }))
}
