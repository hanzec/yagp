<template>
  <section id="node-gost-service" aria-labelledby="node-gost-title">
    <header class="ops-panel-header">
      <h2 class="ops-section-title" id="node-gost-title" tabindex="-1"><el-icon aria-hidden="true"><Monitor /></el-icon>服务状态</h2>
      <el-button class="service-refresh" :icon="Refresh" :loading="statusLoading || firewallLoading || logsLoading" :disabled="restarting" @click="refresh">刷新</el-button>
    </header>

    <div class="service-columns">
      <section class="service-column" aria-labelledby="gost-status-title" :aria-busy="statusLoading || restarting">
        <div class="service-title">
          <h3 class="ops-subsection-title" id="gost-status-title">Gost</h3>
          <span v-if="service && !statusError" class="ops-status" :class="'is-' + statusTone" role="status">{{ statusLabel }}</span>
        </div>
        <p v-if="statusError" class="gost-error" role="alert">{{ statusError }}</p>
        <dl v-else-if="service" class="service-facts">
          <div><dt>后端类型</dt><dd class="ops-mono">systemd</dd></div>
          <div><dt>服务单元</dt><dd class="ops-mono">{{ service.unit || '未知' }}</dd></div>
          <div><dt>加载状态</dt><dd class="ops-mono">{{ service.load_state || '未知' }}</dd></div>
          <div><dt>子状态</dt><dd class="ops-mono">{{ service.sub_state || '未知' }}</dd></div>
        </dl>
        <p v-else class="ops-muted" role="status">
          {{ statusLoading ? '正在读取服务状态…' : '节点当前未在线。可手动刷新以查询服务状态。' }}
        </p>
        <p v-if="restartError" class="gost-error" role="alert">{{ restartError }}</p>

        <div class="gost-actions">
          <el-button
            :icon="SwitchButton"
            type="warning"
            plain
            :loading="restarting"
            :disabled="statusLoading || firewallLoading || logsLoading || !!statusError || service?.load_state !== 'loaded'"
            @click="restart"
          >重启 Gost</el-button>
          <el-button @click="$emit('upgrade')">升级 Gost</el-button>
        </div>
        <div class="gost-log-section">
          <button
            type="button"
            class="gost-log-toggle"
            :aria-expanded="logsOpen"
            aria-controls="node-gost-logs"
            @click="toggleLogs"
          >
            <el-icon aria-hidden="true"><Document /></el-icon>
            <span>服务日志</span>
            <small class="ops-muted">最近 100 条</small>
            <el-icon class="gost-log-chevron" aria-hidden="true"><ArrowDown /></el-icon>
          </button>
          <div v-show="logsOpen" id="node-gost-logs" class="gost-log-content" :aria-busy="logsLoading || restarting">
            <p v-if="logsError" class="gost-error" role="alert">{{ logsError }}</p>
            <p v-else-if="!logs.length" class="ops-muted" role="status">{{ logsLoading || restarting ? '正在读取日志…' : '暂无日志' }}</p>
            <ol v-else class="gost-logs ops-mono" tabindex="0" aria-label="Gost 服务日志，最新记录在前">
              <li v-for="(entry, index) in logs" :key="index">
                <div class="gost-log-meta ops-muted">{{ formatTimestamp(entry.timestamp_unix_micros) }} · {{ priorityLabels[entry.priority ?? 0] || '未知级别' }}</div>
                <div class="gost-log-message">{{ entry.message }}</div>
              </li>
            </ol>
          </div>
        </div>
      </section>

      <section id="node-firewall-status" class="service-column" aria-labelledby="firewall-status-title" :aria-busy="firewallLoading || restarting">
        <div class="service-title">
          <h3 class="ops-subsection-title" id="firewall-status-title">自动防火墙规则</h3>
          <span v-if="firewall && !restarting" class="ops-status firewall-startup" :class="firewallStartup.tone" role="status">{{ firewallStartup.label }}</span>
        </div>
        <p v-if="restarting" class="ops-muted" role="status">Gost 重启后将重新检查防火墙规则。</p>
        <p v-else-if="firewallError" class="gost-error" role="alert">{{ firewallError }}</p>
        <template v-else-if="firewall">
          <dl class="service-facts">
            <div><dt>自动放行后端</dt><dd class="ops-mono">{{ firewallBackend }}</dd></div>
            <div v-if="automaticRules">
              <dt>同步状态</dt>
              <dd><span class="ops-status" :class="firewallSync.tone">{{ firewallSync.label }}</span></dd>
            </div>
            <div v-if="firewall.last_sync"><dt>最近成功同步</dt><dd><time :datetime="firewall.last_sync">{{ formatTime(firewall.last_sync) }}</time></dd></div>
          </dl>
          <p v-if="!automaticRules" class="firewall-reason">
            <el-icon aria-hidden="true"><InfoFilled /></el-icon>
            <span>{{ firewallDisabledReason }}</span>
          </p>
          <p v-else-if="firewallHelp && !firewall.synchronized" class="ops-muted">{{ firewallHelp }}</p>
          <p v-if="firewall.last_error" class="gost-error" role="alert">{{ firewall.last_error }}</p>
        </template>
        <p v-else class="ops-muted" role="status">
          {{ firewallLoading ? '正在读取防火墙状态…' : legacyFirewall ? '守护程序版本较旧，无法查询防火墙状态。' : '节点当前未在线。' }}
        </p>
      </section>
    </div>
  </section>
</template>

<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { ArrowDown, Document, InfoFilled, Monitor, Refresh, SwitchButton } from '@element-plus/icons-vue'
import { formatTime } from '@/utils/node-display'
import { confirmPanelAction } from '@/utils/panel-dialog'
import { getGostLogs, getGostService, getNodeFirewallStatus, restartGostService } from '@/api/node'

const props = defineProps({ node: { type: Object, required: true } })
defineEmits(['upgrade'])
const service = ref(null)
const logs = ref([])
const logsOpen = ref(false)
const statusLoading = ref(false)
const firewall = ref(null)
const firewallLoading = ref(false)
const firewallError = ref('')
const legacyFirewall = ref(false)
const logsLoading = ref(false)
const restarting = ref(false)
const statusError = ref('')
const logsError = ref('')
const restartError = ref('')

const statusLabels = {
  active: '运行中', inactive: '未运行', failed: '失败',
  activating: '正在启动', deactivating: '正在停止', reloading: '正在重新加载'
}
const statusLabel = computed(() => statusLabels[service.value?.active_state] || service.value?.active_state || '未知')
const statusTone = computed(() => ({ active: 'success', failed: 'danger', activating: 'warning' }[service.value?.active_state] || 'muted'))
const priorityLabels = ['紧急', '警报', '严重', '错误', '警告', '通知', '信息', '调试']
const dateFormatter = new Intl.DateTimeFormat('zh-CN', { dateStyle: 'short', timeStyle: 'medium' })
const formatTimestamp = (microseconds) => microseconds ? dateFormatter.format(Number(microseconds) / 1000) : '未知时间'

const firewallBackend = computed(() => {
  const backend = firewall.value?.backend
  return backend === 'ufw' ? 'UFW' : backend || '未识别'
})
const automaticRules = computed(() => firewall.value?.managed && !firewall.value.disabled_reason)
const firewallStartup = computed(() => {
  if (automaticRules.value) return { label: '已启用', tone: 'is-success' }
  if (firewall.value?.disabled_reason === 'checking_firewall') return { label: '正在检查', tone: 'is-warning' }
  if (firewall.value?.disabled_reason === 'waiting_panel') return { label: '等待面板', tone: 'is-muted' }
  if (firewall.value?.disabled_reason === 'no_exit_endpoint') return { label: '未启用', tone: 'is-muted' }
  return { label: '未启用', tone: 'is-warning' }
})
const firewallDisabledReason = computed(() => {
  switch (firewall.value?.disabled_reason) {
    case 'checking_firewall': return '正在重新检查防火墙后端和规则。'
    case 'no_exit_endpoint': return '节点暂无出口端点。添加后自动启用。'
    case 'waiting_panel': return '等待面板下发出口端点状态。'
    case 'explicitly_disabled': return '节点配置将 firewall.enabled 设为 false。'
    case 'not_configured': return '节点尚未配置自动放行。'
    case 'invalid_configuration': return '节点的防火墙配置无效。'
    case 'unsupported_environment': return '当前节点环境不支持自动放行。'
    default: return '节点未启用自动放行。'
  }
})
const firewallHelp = computed(() => {
  switch (firewall.value?.backend) {
    case 'ufw': return '自动放行需要主机上的 UFW 已启用并运行。'
    case 'firewalld': return '自动放行需要 firewalld 已启用并运行，以及管理员配置的入站 policy。'
    case 'nftables': return '管理员在主机 inet input 链中接入 YAGP 放行链。'
    default: return ''
  }
})
const firewallSync = computed(() => {
  if (!firewall.value.supported) return { label: '后端不可用', tone: 'is-danger' }
  if (firewall.value.synchronized) return { label: '已同步', tone: 'is-success' }
  if (firewall.value.last_error) return { label: '同步异常', tone: 'is-danger' }
  return { label: '待同步', tone: 'is-warning' }
})

const readLogs = async () => {
  if (logsLoading.value) return
  logsLoading.value = true
  logsError.value = ''
  try {
    const res = await getGostLogs(props.node.id)
    logs.value = (res.data.entries || []).slice().sort((a, b) => b.timestamp_unix_micros - a.timestamp_unix_micros)
  } catch (error) {
    logsError.value = error.response?.data?.message || '无法读取 Gost 日志，请刷新重试。'
  } finally {
    logsLoading.value = false
  }
}

const readStatus = async () => {
  if (statusLoading.value) return
  statusLoading.value = true
  statusError.value = ''
  try {
    const res = await getGostService(props.node.id)
    service.value = res.data
  } catch (error) {
    statusError.value = error.response?.data?.message || '无法读取 Gost 状态，请刷新重试。'
  } finally {
    statusLoading.value = false
  }
}

let firewallRequest
const readFirewall = (background = false) => {
  if (firewallRequest) return firewallRequest
  firewallLoading.value = true
  if (!background) firewall.value = null
  firewallError.value = ''
  legacyFirewall.value = false
  firewallRequest = getNodeFirewallStatus(props.node.id).then(res => {
    firewall.value = res.data
  }).catch(error => {
    firewall.value = null
    legacyFirewall.value = error.response?.status === 501
    if (!legacyFirewall.value) {
      firewallError.value = error.response?.data?.message || '无法读取防火墙状态，请刷新重试。'
    }
  }).finally(() => {
    firewallRequest = undefined
    firewallLoading.value = false
  })
  return firewallRequest
}

const refresh = () => Promise.all([readStatus(), readFirewall(), logsOpen.value && readLogs()])

const firewallRecheckInterval = 5000
const firewallRecheckLimit = 5
let firewallRecheckTimer
let firewallRecheckGeneration = 0
const cancelFirewallRecheck = () => {
  clearTimeout(firewallRecheckTimer)
  return ++firewallRecheckGeneration
}
const recheckFirewall = (generation, remaining = firewallRecheckLimit) => {
  const awaitingResult = () => generation === firewallRecheckGeneration && props.node.status === 'online' &&
    firewall.value?.disabled_reason === 'checking_firewall'
  if (!remaining || !awaitingResult()) return
  firewallRecheckTimer = setTimeout(async () => {
    if (!awaitingResult()) return
    await readFirewall(true)
    recheckFirewall(generation, remaining - 1)
  }, firewallRecheckInterval)
}

const toggleLogs = () => {
  logsOpen.value = !logsOpen.value
  if (logsOpen.value && !restarting.value) readLogs()
}

const restart = async () => {
  const confirmed = await confirmPanelAction('重启 Gost 会短暂中断该节点正在处理的连接。', '重启 Gost', {
    confirmButtonText: '重启 Gost', cancelButtonText: '取消', type: 'warning'
  }).catch(() => false)
  if (!confirmed) return

  const generation = cancelFirewallRecheck()
  restarting.value = true
  restartError.value = ''
  let succeeded = false
  try {
    const res = await restartGostService(props.node.id)
    service.value = res.data
    succeeded = true
    ElMessage.success('Gost 重启操作已完成')
  } catch (error) {
    restartError.value = error.response?.data?.message || '重启请求失败，请核对服务状态和日志。'
  } finally {
    await firewallRequest
    await refresh()
    restarting.value = false
    if (succeeded) recheckFirewall(generation)
  }
}

watch(() => props.node.status, (status) => {
  if (status === 'online' && !restarting.value) refresh()
}, { immediate: true })

onUnmounted(cancelFirewallRecheck)
</script>

<style scoped>
.service-columns {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(min(100%, 330px), 1fr));
  gap: var(--ops-section-gap);
  padding-top: var(--ops-section-gap);
  margin-top: 12px;
  border-top: 1px solid var(--ops-border);
}
.service-column {
  display: grid;
  align-content: start;
  gap: 12px;
  min-width: 0;
  padding: var(--ops-inset-padding);
  border: 1px solid var(--ops-border);
  border-radius: var(--ops-radius-inset);
  background: var(--ops-inset);
}
.service-title, .gost-actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.service-title .ops-status { flex: none; }
.service-refresh, .gost-actions .el-button { min-height: 44px; margin: 0; }
.service-facts {
  display: grid;
  margin: 0;
  border: 1px solid var(--ops-border);
  border-radius: var(--ops-radius-inset);
  background: var(--ops-panel);
}
.service-facts > div {
  display: grid;
  grid-template-columns: minmax(5em, 1fr) minmax(0, 2fr);
  align-items: baseline;
  gap: 8px;
  padding: 6px 10px;
}
.service-facts > div + div { border-top: 1px solid var(--ops-border); }
.service-facts dt { color: var(--ops-muted); font-size: var(--ops-text-sm); }
.service-facts dd { margin: 0; min-width: 0; overflow-wrap: anywhere; }
.gost-error { color: var(--ops-danger); white-space: pre-wrap; overflow-wrap: anywhere; }
.firewall-reason { display: flex; align-items: baseline; gap: 8px; color: var(--ops-muted); }
.firewall-reason .el-icon { flex: none; align-self: flex-start; margin-top: 4px; }

.gost-log-section { min-width: 0; border-top: 1px solid var(--ops-border); }
.gost-log-toggle {
  display: flex;
  width: 100%;
  align-items: center;
  gap: 8px;
  min-height: 44px;
  padding: 6px 0;
  border: 0;
  border-radius: 4px;
  background: transparent;
  color: var(--ops-text);
  text-align: start;
  font-weight: var(--ops-weight-semibold);
}
.gost-log-toggle:hover { color: var(--ops-accent); }
.gost-log-toggle small { font-size: var(--ops-text-sm); font-weight: var(--ops-weight-normal); }
.gost-log-chevron { margin-inline-start: auto; }
.gost-log-toggle[aria-expanded="true"] .gost-log-chevron { transform: rotate(180deg); }
.gost-log-content { padding-top: 4px; }
.gost-logs {
  max-height: min(40vh, 420px);
  margin: 0;
  padding: 0 12px;
  overflow-y: auto;
  border: 1px solid var(--ops-border);
  border-radius: var(--ops-radius-inset);
  background: var(--ops-inset);
  list-style: none;
}
.gost-logs li { padding: 8px 0; }
.gost-logs li + li { border-top: 1px solid var(--ops-border); }
.gost-log-meta { font-size: var(--ops-text-xs); }
.gost-log-message { font-size: var(--ops-text-sm); margin-top: 4px; white-space: pre-wrap; overflow-wrap: anywhere; }
</style>
