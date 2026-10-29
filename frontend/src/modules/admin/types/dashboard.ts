// 仪表盘统计相关类型定义。
// 这些指标语义与「我的站点」概览保持一致，后续会由后端统计接口提供真实数据。

/** 趋势图统计周期：北京时间本周（周一至今）/ 本月（1 日至今）。 */
export type DashboardPeriod = 'week' | 'month'

/** 仪表盘的五个核心指标。 */
export type DashboardMetricKey =
  | 'todayProfit' // 今日营收
  | 'siteBalance' // 站点用户总余额
  | 'todayPurchase' // 今日成本
  | 'netProfit' // 今日净利润
  | 'upstreamBalance' // 上游总余额

/** 可用的主题色 token，对应 globals.css / tailwind.config 中的 CSS 变量。 */
export type DashboardColorToken = 'primary' | 'accent' | 'signal' | 'warning'

/** 单个趋势数据点。 */
export interface TrendPoint {
  /** X 轴标签，使用与语言无关的日期格式，如 "6/27"。 */
  label: string
  /** null 表示当天缺失或尚未确认，图表不得补零。 */
  value: number | null
}

/** 同一指标在不同周期下的连续序列。 */
export interface MetricSeries {
  week: TrendPoint[]
  month: TrendPoint[]
}

/** 一个仪表盘指标的完整数据（当前值 + 连续趋势序列）。 */
export interface DashboardMetricData {
  key: DashboardMetricKey
  color: DashboardColorToken
  /** 当前值（“今天”的数值），即月序列最后一个点。 */
  current: number
  /** 当前值是否可信；实时读取失败（如站点余额拉取失败）时为 false，页面显示未知而不是 0。 */
  currentKnown: boolean
  /** 北京时间昨日与今日的日对比；昨日缺失或未确认时仅含今日，不随自然周或自然月截断。 */
  comparison: (TrendPoint & { value: number })[]
  series: MetricSeries
  /** 最近几个完整自然日（不含今天）的历史值，供预计续航等不随周期切换的估算使用。 */
  recent: TrendPoint[]
}
