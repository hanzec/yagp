<template>
  <div class="ops-page tunnel-detail">
    <Teleport :to="headingTarget" :disabled="!headingTarget">
      <nav aria-label="隧道详情导航">
        <h1 class="tunnel-detail-heading">
          <span class="tunnel-detail-parent">
            <RouterLink :to="{ name: 'Tunnels' }" class="tunnel-back">返回隧道列表</RouterLink>
            <span aria-hidden="true">/</span>
          </span>
          <span class="tunnel-detail-name" :title="tunnel?.name" aria-current="page">{{ tunnel?.name || route.meta.title }}</span>
        </h1>
      </nav>
    </Teleport>

    <div v-if="tunnelError" class="ops-panel ops-panel-body tunnel-feedback" role="alert">
      <span>{{ tunnelError }}</span>
      <el-button v-if="!tunnelMissing" @click="loadTunnel">重新加载</el-button>
    </div>
    <div v-else-if="!tunnel" class="ops-panel ops-panel-body tunnel-placeholder" role="status">正在加载隧道信息…</div>

    <template v-if="tunnel">
      <section class="ops-panel ops-panel-body tunnel-overview" aria-labelledby="tunnel-overview-title">
        <header class="ops-panel-header">
          <div class="tunnel-identity">
            <h2 id="tunnel-overview-title" class="ops-section-title">
              <el-icon aria-hidden="true"><Connection /></el-icon> 隧道拓扑
            </h2>
            <span class="ops-muted ops-mono">#{{ tunnel.id }}</span>
            <span class="ops-status" :class="'is-' + tunnelState.tone">{{ tunnelState.label }}</span>
          </div>
        </header>
        <TunnelTopology v-if="paths.length > 1" :tunnels="paths" />
        <div v-else class="tunnel-node-pair" aria-label="转发方向" role="group">
          <dl class="ops-inset">
            <dt>入口节点</dt>
            <dd>{{ tunnel.entry_node?.name || '节点 #' + tunnel.entry_node_id }}</dd>
          </dl>
          <el-icon aria-hidden="true"><Right /></el-icon>
          <dl class="ops-inset">
            <dt>出口节点</dt>
            <dd>{{ tunnel.exit_node?.name || '节点 #' + tunnel.exit_node_id }}</dd>
          </dl>
        </div>
        <p v-if="tunnel.remark" class="tunnel-remark"><span class="ops-muted">备注</span> {{ tunnel.remark }}</p>
      </section>

      <section class="ops-panel ops-panel-body tunnel-endpoints" aria-labelledby="tunnel-endpoints-title">
        <header class="ops-panel-header">
          <div class="tunnel-identity">
            <h2 id="tunnel-endpoints-title" class="ops-section-title">端点配置</h2>
            <span v-if="connections.length > 1" class="ops-muted">{{ connections.length }} 组端点</span>
          </div>
        </header>
        <div v-for="connection in connections" :key="connection.id" class="tunnel-endpoint-details ops-inset">
          <h3 v-if="paths.length > 1">{{ connection.direction }}</h3>
          <dl class="ops-panel-details tunnel-metadata">
            <div v-if="connection.entry_rule">
              <dt>入口端点</dt>
              <dd>{{ connection.entry_rule.name }}</dd>
              <dd class="ops-muted ops-mono">{{ formatEndpoint(connection.entry_rule.listen_ip, connection.entry_rule.listen_port) }}</dd>
            </div>
            <div v-if="connection.exit_rule"><dt>出口端点</dt><dd>{{ connection.exit_rule.name }}</dd></div>
            <div><dt>Relay 监听</dt><dd class="ops-mono">{{ connection.protocol || '—' }} <template v-if="connection.port">· {{ formatEndpoint(connection.listenIP, connection.port) }}</template></dd></div>
            <div v-if="connection.connectEndpoint"><dt>入口连接的 Relay 地址</dt><dd class="ops-mono">{{ connection.connectEndpoint }}</dd></div>
          </dl>
        </div>
      </section>

      <section class="ops-panel ops-panel-body tunnel-monitor" aria-labelledby="tunnel-monitor-title" :aria-busy="metricsLoading">
        <header class="ops-panel-header">
          <div>
            <h2 id="tunnel-monitor-title" class="ops-section-title">
              <el-icon aria-hidden="true"><TrendCharts /></el-icon> 链路质量
            </h2>
            <p v-if="metrics" class="tunnel-caption">
              {{ connections.length > 1 ? '汇总全部端点的探测结果 · ' : '' }}运行时每 {{ metrics.interval_seconds }} 秒探测一次
              <template v-if="latestProbe"> · 最近探测 {{ latestProbe }}</template>
            </p>
          </div>
          <div class="tunnel-ranges" role="group" aria-label="统计时间范围">
            <button v-for="{ value, label } in ranges" :key="value" type="button" :aria-pressed="range === value" @click="range = value">{{ label }}</button>
          </div>
        </header>

        <div v-if="metricsError" class="tunnel-feedback" role="alert">
          <span>{{ metrics ? '探测记录更新失败，当前显示上次获取的数据。' : '无法加载探测记录。' }}</span>
          <el-button @click="loadMetrics">重试</el-button>
        </div>

        <div v-if="!metrics && metricsLoading" class="tunnel-placeholder" role="status">正在加载探测记录…</div>
        <template v-else-if="metrics">
          <dl class="tunnel-metrics">
            <div v-for="item in metricsCards" :key="item.label" class="ops-inset">
              <dt class="ops-muted">{{ item.label }}</dt>
              <dd class="ops-mono">{{ item.value }} <small v-if="item.value !== '—'">{{ item.unit }}</small></dd>
              <dd class="tunnel-caption">{{ item.note }}</dd>
            </div>
          </dl>
          <TunnelLatencyChart :metrics="metrics" />
          <p class="tunnel-caption tunnel-measurement-note">
            RTT 由 Gost Relay 内的数据往返测得。TTL 取自出口 Relay 地址的 ICMP 回包，IPv6 对应 Hop Limit。不可用的探测不计入失败率。
          </p>
        </template>
      </section>
    </template>
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Connection, Right, TrendCharts } from '@element-plus/icons-vue'
import { getTunnel, getTunnelMetrics } from '@/api/tunnel'
import TunnelLatencyChart from '@/components/TunnelLatencyChart.vue'
import TunnelTopology from '@/components/TunnelTopology.vue'
import { formatEndpoint, mappedConnectEndpoint } from '@/utils/node-address-mapping'
import { tunnelExitConfig } from '@/utils/rule-state'
import { connectionsOf, tunnelPaths } from '@/utils/tunnel-inventory'

defineProps({ headingTarget: { type: Object, default: null } })

const route = useRoute()
const router = useRouter()
const ranges = [
  { value: '1h', label: '1 小时' },
  { value: '6h', label: '6 小时' },
  { value: '24h', label: '24 小时' },
  { value: '7d', label: '7 天' },
  { value: '30d', label: '30 天' }
]
const range = ref('24h')
const tunnel = ref(null)
const metrics = ref(null)
const tunnelError = ref('')
const tunnelMissing = ref(false)
const metricsError = ref(false)
const metricsLoading = ref(false)
let tunnelRequest
let metricsRequest
let refreshTimer

const states = {
  running: { label: '运行中', tone: 'success' },
  stopped: { label: '已停止', tone: 'muted' },
  error: { label: '异常', tone: 'danger' }
}
const tunnelState = computed(() => states[tunnel.value?.status] || { label: '未知', tone: 'muted' })
const paths = computed(() => tunnel.value ? tunnelPaths(tunnel.value) : [])
const connections = computed(() => {
  if (!tunnel.value) return []
  return connectionsOf(tunnel.value).map(connection => {
    const exit = tunnelExitConfig(connection)
    const entryNode = connection.entry_node ||
      (!connection.entry_node_id || connection.entry_node_id === tunnel.value.entry_node_id ? tunnel.value.entry_node : null)
    const exitNode = connection.exit_node ||
      (!connection.exit_node_id || connection.exit_node_id === tunnel.value.exit_node_id ? tunnel.value.exit_node : null)
    const listenIP = exit?.listen_ip
    const port = exit?.listen_port || connection.relay_port
    const host = listenIP || exitNode?.address
    return {
      id: connection.id,
      direction: `${entryNode?.name || '节点 #' + connection.entry_node_id} → ${exitNode?.name || '节点 #' + connection.exit_node_id}`,
      entry_rule: connection.entry_rule,
      exit_rule: connection.exit_rule,
      protocol: (exit?.role === 'exit' ? exit.protocol : connection.protocol)?.toUpperCase(),
      listenIP,
      port,
      connectEndpoint: mappedConnectEndpoint(exitNode, listenIP, port) || (host && port ? formatEndpoint(host, port) : '')
    }
  })
})

const numberFormat = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 1 })
const clockFormat = new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' })
const shownNumber = value => value == null ? '—' : numberFormat.format(value)
const latestProbe = computed(() => {
  const summary = metrics.value?.summary
  if (!summary?.latest_probe_at) return ''
  const status = { successful: '成功', failed: '失败', unavailable: '不可用' }[summary.latest_status]
  return [clockFormat.format(new Date(summary.latest_probe_at)), status].filter(Boolean).join(' · ')
})
const metricsCards = computed(() => {
  const summary = metrics.value.summary
  const measured = summary.successful + summary.failed
  return [
    { label: '最新 RTT', value: shownNumber(summary.latest_latency_ms), unit: 'ms', note: summary.successful ? '最近一次成功探测' : '暂无成功样本' },
    { label: '平均 RTT', value: shownNumber(summary.avg_latency_ms), unit: 'ms', note: summary.successful ? `范围 ${shownNumber(summary.min_latency_ms)}–${shownNumber(summary.max_latency_ms)} ms` : '暂无成功样本' },
    { label: 'P95 RTT', value: shownNumber(summary.p95_latency_ms), unit: 'ms', note: `${summary.successful} 次成功` },
    { label: '失败率', value: measured ? (summary.failed * 100 / measured).toFixed(2) : '—', unit: '%', note: `${summary.failed} 次失败 · ${summary.unavailable} 次不可用` },
    { label: '最近出口 TTL', value: shownNumber(summary.latest_ttl), note: summary.latest_ttl == null ? '暂无 ICMP 回包读数' : 'ICMP 回包' }
  ]
})

async function loadTunnel() {
  if (tunnelRequest) return
  const request = new AbortController()
  tunnelRequest = request
  try {
    const { data } = await getTunnel(route.params.id, request.signal)
    if (request.signal.aborted) return
    if (String(data.id) !== route.params.id) {
      await router.replace({ name: 'TunnelDetail', params: { id: data.id } })
      return
    }
    tunnel.value = data
    tunnelError.value = ''
    tunnelMissing.value = false
  } catch (error) {
    if (request.signal.aborted) return
    tunnelMissing.value = error.response?.status === 404
    if (tunnelMissing.value) tunnel.value = null
    tunnelError.value = tunnelMissing.value ? '该隧道不存在。' : '无法加载隧道信息。'
  } finally {
    if (tunnelRequest === request) tunnelRequest = null
  }
}

async function loadMetrics() {
  if (metricsRequest) return
  const request = new AbortController()
  metricsRequest = request
  metricsLoading.value = true
  try {
    const { data } = await getTunnelMetrics(route.params.id, range.value, request.signal)
    if (request.signal.aborted) return
    metrics.value = data
    metricsError.value = false
  } catch {
    if (!request.signal.aborted) metricsError.value = true
  } finally {
    if (metricsRequest === request) {
      metricsRequest = null
      metricsLoading.value = false
    }
  }
}

watch(() => route.params.id, () => {
  tunnelRequest?.abort()
  tunnelRequest = null
  tunnel.value = null
  tunnelError.value = ''
  tunnelMissing.value = false
  loadTunnel()
}, { immediate: true })

watch([() => route.params.id, range], ([id], [previousId]) => {
  metricsRequest?.abort()
  metricsRequest = null
  if (id !== previousId) metrics.value = null
  metricsError.value = false
  loadMetrics()
}, { immediate: true })

onMounted(() => {
  refreshTimer = setInterval(() => {
    if (document.hidden || tunnelMissing.value) return
    loadTunnel()
    loadMetrics()
  }, 60000)
})

onUnmounted(() => {
  clearInterval(refreshTimer)
  tunnelRequest?.abort()
  metricsRequest?.abort()
})
</script>

<style scoped>
.tunnel-detail-heading, .tunnel-detail-parent, .tunnel-back { display: flex; align-items: center; }
.tunnel-detail-heading { flex-wrap: wrap; column-gap: 8px; font-size: var(--ops-text-title); white-space: nowrap; }
.tunnel-detail-parent { gap: 8px; }
.tunnel-detail-parent > span { color: var(--ops-muted); }
.tunnel-back { min-height: 44px; color: var(--ops-text); text-decoration: none; }
.tunnel-back:hover { color: var(--ops-accent); text-decoration: underline; }
.tunnel-detail-name { min-width: 0; overflow: hidden; text-overflow: ellipsis; }
.tunnel-identity { display: flex; align-items: center; flex-wrap: wrap; gap: 8px 12px; }
.tunnel-overview, .tunnel-endpoints, .tunnel-monitor { display: grid; grid-template-columns: minmax(0, 1fr); gap: var(--ops-section-gap); }
.tunnel-node-pair { display: grid; grid-template-columns: minmax(0, 1fr) auto minmax(0, 1fr); align-items: center; gap: 12px; }
.tunnel-node-pair dl { margin: 0; padding: 12px; overflow-wrap: anywhere; }
.tunnel-node-pair dt { color: var(--ops-muted); font-size: var(--ops-text-sm); }
.tunnel-node-pair dd { margin: 4px 0 0; font-weight: var(--ops-weight-semibold); }
.tunnel-node-pair > .el-icon { color: var(--ops-accent); }
.tunnel-endpoint-details { padding: 12px; }
.tunnel-endpoint-details h3 { margin: 0 0 10px; font-size: var(--ops-text-base); overflow-wrap: anywhere; }
.tunnel-metadata { grid-template-columns: repeat(auto-fit, minmax(min(100%, 150px), 1fr)); }
.tunnel-metadata dd { overflow-wrap: anywhere; }
.tunnel-metadata dd.ops-muted { font-size: var(--ops-text-sm); }
.tunnel-remark { overflow-wrap: anywhere; }
.tunnel-remark span { margin-inline-end: 8px; }
.tunnel-caption { color: var(--ops-muted); font-size: var(--ops-text-sm); }
.tunnel-ranges { display: flex; min-width: 0; flex-wrap: wrap; gap: 4px; }
.tunnel-ranges button { min-width: 44px; min-height: 44px; padding: 0 10px; border: 1px solid var(--ops-border); border-radius: var(--ops-radius-inset); background: var(--ops-inset); color: var(--ops-muted); }
.tunnel-ranges button:hover { border-color: var(--ops-accent-line); color: var(--ops-text); }
.tunnel-ranges button[aria-pressed="true"] { border-color: var(--ops-accent-line); background: var(--ops-accent-bg); color: var(--ops-accent); font-weight: var(--ops-weight-semibold); }
.tunnel-metrics { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 140px), 1fr)); gap: 8px; margin: 0; }
.tunnel-metrics > div { display: grid; align-content: start; gap: 4px; padding: 12px; }
.tunnel-metrics dt { font-size: var(--ops-text-sm); }
.tunnel-metrics dd { margin: 0; }
.tunnel-metrics dd.ops-mono { font-size: clamp(1.3rem, 2.2vw, 1.65rem); line-height: 1.35; }
.tunnel-metrics small { color: var(--ops-muted); font-size: var(--ops-text-sm); }
.tunnel-placeholder { display: grid; place-content: center; gap: 4px; min-height: 148px; padding: 16px; color: var(--ops-muted); text-align: center; }
.tunnel-feedback { display: flex; align-items: center; flex-wrap: wrap; gap: 12px; color: var(--ops-warning); }
.tunnel-feedback .el-button { min-height: 44px; }
.tunnel-measurement-note { border-top: 1px solid var(--ops-border); padding-top: 12px; }
@media (max-width: 400px) {
  .tunnel-ranges button { padding-inline: 7px; }
  .tunnel-metrics { grid-template-columns: repeat(2, minmax(0, 1fr)); }
}
</style>
