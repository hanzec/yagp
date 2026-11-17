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

    <div v-if="tunnel" class="tunnel-identity">
      <span class="ops-status" :class="'is-' + tunnelState.tone">{{ tunnelState.label }}</span>
      <span class="ops-muted ops-mono">#{{ tunnel.id }}</span>
    </div>

    <div v-if="tunnelError" class="ops-panel ops-panel-body tunnel-feedback" role="alert">
      <span>{{ tunnelError }}</span>
      <el-button v-if="!tunnelMissing" @click="loadTunnel">重新加载</el-button>
    </div>
    <div v-else-if="!tunnel" class="ops-panel ops-panel-body tunnel-placeholder" role="status">正在加载隧道信息…</div>

    <template v-if="tunnel">
      <section class="ops-panel ops-panel-body tunnel-overview" aria-labelledby="tunnel-overview-title">
        <h2 id="tunnel-overview-title" class="ops-section-title">
          <el-icon aria-hidden="true"><Connection /></el-icon> 隧道信息
        </h2>
        <div class="tunnel-topology">
          <template v-for="(endpoint, index) in endpoints" :key="endpoint.label">
            <div class="ops-inset tunnel-endpoint" :class="{ 'tunnel-relay': index === 1 }">
              <span class="ops-muted">{{ endpoint.label }}</span>
              <strong>{{ endpoint.name }}</strong>
              <small v-if="endpoint.detail" class="ops-mono">{{ endpoint.detail }}</small>
            </div>
            <div v-if="index < endpoints.length - 1" class="tunnel-topology-link" aria-hidden="true"><el-icon><Right /></el-icon></div>
          </template>
        </div>
        <dl v-if="connectEndpoint || tunnel.remark" class="ops-panel-details tunnel-metadata">
          <div v-if="connectEndpoint"><dt>入口连接的 Relay 地址</dt><dd class="ops-mono">{{ connectEndpoint }}</dd></div>
          <div v-if="tunnel.remark"><dt>备注</dt><dd>{{ tunnel.remark }}</dd></div>
        </dl>
      </section>

      <section class="ops-panel ops-panel-body tunnel-monitor" aria-labelledby="tunnel-monitor-title" :aria-busy="!metrics && metricsLoading">
        <header class="ops-panel-header">
          <div>
            <h2 id="tunnel-monitor-title" class="ops-section-title">
              <el-icon aria-hidden="true"><TrendCharts /></el-icon> 链路质量
            </h2>
            <p v-if="metrics" class="tunnel-caption">
              运行时每 {{ metrics.interval_seconds }} 秒探测一次
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
        <div v-else-if="metrics && !metrics.summary.attempts" class="tunnel-placeholder" role="status">
          <strong>此时间范围内暂无探测记录</strong>
          <span>{{ tunnel.status === 'running' ? '请等待下一轮探测，或切换时间范围。' : '隧道运行时会自动采集探测记录。' }}</span>
        </div>
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
import { useRoute } from 'vue-router'
import { Connection, Right, TrendCharts } from '@element-plus/icons-vue'
import { getTunnel, getTunnelMetrics } from '@/api/tunnel'
import TunnelLatencyChart from '@/components/TunnelLatencyChart.vue'
import { formatEndpoint, mappedConnectEndpoint } from '@/utils/node-address-mapping'

defineProps({ headingTarget: { type: Object, default: null } })

const route = useRoute()
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
const relayProtocol = computed(() => (tunnel.value.exit_rule?.role === 'exit' ? tunnel.value.exit_rule.protocol : tunnel.value.protocol)?.toUpperCase())
const relayPort = computed(() => tunnel.value.exit_rule?.listen_port || tunnel.value.relay_port)
const relayListenIP = computed(() => tunnel.value.exit_rule?.listen_ip)
const connectEndpoint = computed(() => {
  const node = tunnel.value.exit_node
  const host = relayListenIP.value || node?.address
  const port = relayPort.value
  return mappedConnectEndpoint(node, relayListenIP.value, port) || (host && port ? formatEndpoint(host, port) : '')
})

const endpoints = computed(() => {
  const row = tunnel.value
  const entryRule = row.entry_rule
  return [
    {
      label: '入口节点',
      name: row.entry_node?.name || '节点 #' + row.entry_node_id,
      detail: entryRule ? `${entryRule.name} · ${formatEndpoint(entryRule.listen_ip, entryRule.listen_port)}` : row.entry_node?.address
    },
    {
      label: 'Relay',
      name: relayProtocol.value || '—',
      detail: relayPort.value ? formatEndpoint(relayListenIP.value, relayPort.value) : ''
    },
    {
      label: '出口节点',
      name: row.exit_node?.name || '节点 #' + row.exit_node_id,
      detail: row.exit_rule?.name || row.exit_node?.address
    }
  ]
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

watch([() => route.params.id, range], () => {
  metricsRequest?.abort()
  metricsRequest = null
  metrics.value = null
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
.tunnel-overview, .tunnel-monitor { display: grid; gap: var(--ops-section-gap); }
.ops-section-title { display: flex; align-items: center; gap: 8px; }
.ops-section-title .el-icon { color: var(--ops-accent); }
.tunnel-topology { display: grid; grid-template-columns: minmax(0, 1fr) 30px minmax(112px, .65fr) 30px minmax(0, 1fr); }
.tunnel-endpoint { display: grid; align-content: start; gap: 4px; overflow-wrap: anywhere; }
.tunnel-relay { text-align: center; }
.tunnel-endpoint span, .tunnel-endpoint small { color: var(--ops-muted); font-size: var(--ops-text-sm); }
.tunnel-endpoint strong { font-size: var(--ops-text-base); }
.tunnel-topology-link { display: flex; align-items: center; color: var(--ops-accent); }
.tunnel-topology-link::before { flex: 1; border-top: 1px dashed var(--ops-accent-line); content: ''; }
.tunnel-metadata { grid-template-columns: repeat(auto-fit, minmax(min(100%, 230px), 1fr)); }
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
.tunnel-placeholder strong { color: var(--ops-text); }
.tunnel-feedback { display: flex; align-items: center; flex-wrap: wrap; gap: 12px; color: var(--ops-warning); }
.tunnel-feedback .el-button { min-height: 44px; }
.tunnel-measurement-note { border-top: 1px solid var(--ops-border); padding-top: 12px; }
@media (max-width: 640px) {
  .tunnel-topology { grid-template-columns: 1fr; gap: 8px; }
  .tunnel-relay { text-align: start; }
  .tunnel-topology-link { display: none; }
}
@media (max-width: 400px) {
  .tunnel-ranges button { padding-inline: 7px; }
  .tunnel-metrics { grid-template-columns: repeat(2, minmax(0, 1fr)); }
}
</style>
