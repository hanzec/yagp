<template>
  <details id="node-firewall-setup" class="firewall-setup">
    <summary>
      <el-icon aria-hidden="true"><Document /></el-icon>
      {{ repair ? '查看配置步骤' : '查看开启步骤' }}
      <el-icon class="setup-chevron" aria-hidden="true"><ArrowDown /></el-icon>
    </summary>
    <div class="ops-inset setup-content">
      <p v-if="backend === 'nftables'" class="ops-muted">节点使用 UFW 时，可先运行 <code>sudo ufw status</code> 确认它正在运行，再核对节点是否已指定 nftables 表或入站基链。</p>
      <ol class="setup-steps">
        <li v-if="backend === 'ufw'">登录节点，运行 <code>sudo ufw status</code> 确认 UFW 为 <code>active</code>。systemd 主机还需用 <code>systemctl is-enabled ufw.service</code> 确认开机启用。</li>
        <li v-else-if="backend === 'firewalld'">登录节点，确认 firewalld 正在运行，并按文档为该节点创建专用入站 policy。</li>
        <li v-else>登录节点，确认已有 nftables <code>inet</code> 表和入站基链，按文档接入该节点的 <code>yagp_{{ nodeId }}_input</code> 放行链。</li>
        <li>
          编辑节点的 <code>{{ configPath }}</code>，将以下内容合并到现有配置。
          <p class="ops-muted">自定义安装请使用守护程序的实际配置路径。{{ configHint }}</p>
          <pre><code>{{ yaml }}</code></pre>
        </li>
        <li>
          重启节点的 <code>yagp-daemon</code>，例如执行 <code>{{ restartCommand }}</code>。节点重新在线后，点击「刷新」确认同步状态。
          <p v-if="!gostUnit" class="ops-muted">OpenRC 安装可运行 <code>sudo rc-service yagp-daemon-{{ nodeId }} restart</code>。</p>
        </li>
      </ol>
      <a :href="documentation" target="_blank" rel="noopener noreferrer">查看防火墙配置文档</a>
    </div>
  </details>
</template>

<script setup>
import { computed } from 'vue'
import { ArrowDown, Document } from '@element-plus/icons-vue'

const props = defineProps({
  backend: { type: String, required: true },
  nodeId: { type: Number, required: true },
  gostUnit: { type: String, default: '' },
  repair: { type: Boolean, default: false }
})

const legacyLayout = computed(() => props.gostUnit === `yagp-gost-${props.nodeId}.service`)
const configPath = computed(() => legacyLayout.value ? `/etc/yagp-daemon/${props.nodeId}/node.yaml` : '/etc/yagp-daemon/node.yaml')
const restartCommand = computed(() => `sudo systemctl restart yagp-daemon${legacyLayout.value ? `-${props.nodeId}` : ''}`)
const documentation = 'https://github.com/hanzec/yagp/blob/master/README.md#节点防火墙端口放行'
const yaml = computed(() => {
  const enabled = 'firewall:\n  enabled: true'
  if (props.backend === 'firewalld') return `${enabled}\n  policy: yagp-${props.nodeId}`
  if (props.backend === 'nftables') return `${enabled}\n  table: filter\n  input_chain: input`
  return enabled
})
const configHint = computed(() => ({
  firewalld: '将 policy 填为已创建的名称。',
  nftables: '将 filter 和 input 换成节点实际的表名和入站基链名。'
}[props.backend]))
</script>

<style scoped>
.firewall-setup { min-width: 0; }
.firewall-setup summary {
  display: flex;
  align-items: center;
  gap: 8px;
  min-height: 44px;
  color: var(--ops-accent);
  font-weight: var(--ops-weight-semibold);
  cursor: pointer;
}
.firewall-setup summary::-webkit-details-marker { display: none; }
.setup-chevron { margin-inline-start: auto; }
.firewall-setup[open] .setup-chevron { transform: rotate(180deg); }
.setup-content { display: grid; gap: 8px; }
.setup-content a { justify-self: start; }
.setup-steps { display: grid; gap: 8px; margin: 0; padding-inline-start: 1.5em; }
.setup-steps li { overflow-wrap: anywhere; }
.setup-steps code { color: var(--ops-text); }
.setup-steps pre {
  padding: 8px 12px;
  margin: 8px 0 0;
  border-radius: var(--ops-radius-inset);
  background: var(--ops-rail);
  white-space: pre-wrap;
}
</style>
