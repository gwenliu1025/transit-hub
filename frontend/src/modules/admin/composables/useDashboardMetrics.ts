// 仪表盘指标数据来源。
//
// 从后端 /api/dashboard/metrics 获取实时指标，
// 从 /api/dashboard/trends 获取历史快照，两者组合后驱动统计卡片与趋势图。

import { ref } from 'vue'
import type { DashboardMetricData } from '../types/dashboard'
import { buildDashboardMetrics } from '../utils/dashboardMetrics'
import {
  getDashboardMetrics,
  getDashboardTrends,
  type DashboardMetricsResponse,
  type DashboardTrendsResponse,
} from '../api/dashboardAdmin'

export function useDashboardMetrics() {
  const metrics = ref<DashboardMetricData[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)

  const fetchMetrics = async () => {
    loading.value = true
    error.value = null
    try {
      const [live, trends] = await Promise.all([
        getDashboardMetrics(),
        getDashboardTrends(30),
      ])

      applyRawData(live, trends)
    } catch (err) {
      error.value = err instanceof Error ? err.message : 'admin.dashboard.loadError'
    } finally {
      loading.value = false
    }
  }

  const applyRawData = (live: DashboardMetricsResponse, trends: DashboardTrendsResponse) => {
    metrics.value = buildDashboardMetrics(live, trends.points)
  }

  return { metrics, loading, error, fetchMetrics, applyRawData }
}
