// 仪表盘共用的展示工具：主题色类名映射、CNY 金额格式化、环比变化计算。
// 颜色类使用「字面量字符串」写法，确保 Tailwind JIT 能扫描到对应工具类。

import type { DashboardColorToken } from '../types/dashboard'

/** 指标图标底色 + 文字色。 */
export const METRIC_ICON_CLASSES: Record<DashboardColorToken, string> = {
  primary: 'bg-primary/10 text-primary',
  accent: 'bg-accent/10 text-accent',
  signal: 'bg-signal/10 text-signal',
  warning: 'bg-warning/10 text-warning',
}

/** 趋势卡标题前的小圆点颜色。 */
export const METRIC_DOT_CLASSES: Record<DashboardColorToken, string> = {
  primary: 'bg-primary',
  accent: 'bg-accent',
  signal: 'bg-signal',
  warning: 'bg-warning',
}

/** 环比变化方向。 */
export type DeltaDirection = 'up' | 'down' | 'flat'

/** 不同方向的文字色（红色做了暗黑模式适配）。 */
export const DELTA_TEXT_CLASSES: Record<DeltaDirection, string> = {
  up: 'text-signal',
  down: 'text-red-500 dark:text-red-400',
  flat: 'text-muted-foreground',
}

// 固定使用 en-US 千分位分组，只影响数字分隔符（无本地化文字），保证两种语言下表现一致。
const cnyFormatter = new Intl.NumberFormat('en-US', {
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
})

/** 格式化为人民币显示，空值返回占位符。 */
export function formatCny(value: number | null | undefined): string {
  if (value == null || !Number.isFinite(value)) return '¥—'
  // 负数显示为 -¥17.00；四舍五入后为 0 的微小负数不显示负号。
  const cents = Math.round(value * 100) / 100
  if (cents < 0) return `-¥${cnyFormatter.format(-cents)}`
  return `¥${cnyFormatter.format(Math.abs(cents))}`
}

/** 把毫秒时间戳格式化为可读时间；空值或非数字返回 null，由调用方回退「未知」文案。 */
export function formatDateTime(ms: number | null | undefined, locale = 'zh-CN'): string | null {
  if (ms == null || !Number.isFinite(ms)) return null
  return new Intl.DateTimeFormat(locale, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(new Date(ms))
}

export interface DeltaResult {
  amount: number
  direction: DeltaDirection
}

/** 计算序列最后一个点相对前一个点的变化（今日 vs 昨日）。 */
export function computeDelta(values: number[]): DeltaResult {
  if (values.length < 2) return { amount: 0, direction: 'flat' }
  // 先按分取整再判断方向，避免 0.1+0.2 对比 0.3 这类浮点误差显示成“↗¥0.00”。
  const amount = Math.round((values[values.length - 1] - values[values.length - 2]) * 100) / 100
  const direction: DeltaDirection = amount > 0 ? 'up' : amount < 0 ? 'down' : 'flat'
  return { amount: amount === 0 ? 0 : amount, direction }
}
