<template>
  <div class="network-topology" :class="{ 'is-compact': compact }" data-testid="tunnel-topology">
    <div class="network-topology-toolbar">
      <div class="network-view-controls" role="group" aria-label="拓扑图显示大小">
        <button type="button" aria-label="适应拓扑图宽度" :aria-pressed="viewMode === 'fit'" @click="viewMode = 'fit'">适应宽度</button>
        <button type="button" aria-label="按原始大小查看拓扑图" :aria-pressed="viewMode === 'actual'" @click="viewMode = 'actual'">原始大小</button>
      </div>
    </div>
    <div ref="viewport" class="network-topology-scroll" tabindex="0" role="region" aria-label="隧道拓扑图，可滚动查看">
      <svg :viewBox="`0 0 ${layout.width} ${layout.height}`" :width="layout.width" :height="layout.height" :style="{ width: displayWidth + 'px', height: 'auto' }" role="img" aria-label="隧道节点与转发方向">
        <desc>{{ layout.nodes.length }} 个物理节点，{{ layout.links.length }} 条有向链路。箭头从入口指向出口。</desc>
        <defs>
          <marker :id="arrowId" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" class="network-arrow" /></marker>
        </defs>
        <g v-for="column in layout.columns" :key="column.index" class="network-column-heading" :data-column-index="column.index">
          <title>{{ column.title }}</title>
          <text :x="column.centerX" y="25" text-anchor="middle">{{ compact ? column.shortTitle : column.title }}</text>
        </g>
        <g v-for="link in layout.links" :key="link.id" class="network-edge" :data-link-id="link.id">
          <title>{{ link.source.name }} → {{ link.target.name }}</title>
          <path :d="link.path" class="network-edge-line" :marker-end="`url(#${arrowId})`" />
        </g>
        <g v-for="node in layout.nodes" :key="node.id" :data-node-id="node.id" :data-node-column="node.column" class="network-node" :transform="`translate(${node.x}, ${node.y})`">
          <title>{{ node.name }}，节点 #{{ node.id }}，{{ node.incoming + node.outgoing }} 条关联链路</title>
          <rect :width="node.width" :height="node.height" rx="8" />
          <foreignObject :x="compact ? 4 : 12" y="10" :width="node.width - (compact ? 8 : 24)" :height="node.height - 20">
            <div xmlns="http://www.w3.org/1999/xhtml" class="network-node-copy">
              <span class="network-node-role">{{ node.incoming && node.outgoing ? '中继' : node.outgoing ? '入口' : '出口' }}</span>
              <strong :title="node.name">{{ node.name }}</strong>
              <span class="network-node-meta">#{{ node.id }}<template v-if="!compact"> · {{ node.incoming + node.outgoing }} 条链路</template></span>
            </div>
          </foreignObject>
        </g>
      </svg>
    </div>
    <details class="network-connections" :open="displayWidth / layout.width < .85">
      <summary>转发路径 · {{ layout.links.length }} 条</summary>
      <ul>
        <li v-for="link in layout.links" :key="link.id">
          {{ link.source.name }} → {{ link.target.name }}
        </li>
      </ul>
    </details>
  </div>
</template>

<script setup>
import { computed, onBeforeUnmount, onMounted, ref, useId } from 'vue'
import { layoutTunnelTopology } from '@/utils/tunnel-topology'

const props = defineProps({
  tunnels: { type: Array, required: true }
})
const markerId = useId().replace(/[^a-zA-Z0-9-]/g, '')
const arrowId = `topology-arrow-${markerId}`
const viewport = ref(null)
const availableWidth = ref(0)
const viewMode = ref('fit')
const compact = computed(() => availableWidth.value > 0 && availableWidth.value < 600)
const layout = computed(() => layoutTunnelTopology(props.tunnels, compact.value))
const displayWidth = computed(() => viewMode.value === 'fit' && availableWidth.value
  ? Math.min(layout.value.width, availableWidth.value) : layout.value.width)
let observer
onMounted(() => {
  availableWidth.value = viewport.value?.clientWidth || 0
  if (typeof ResizeObserver === 'undefined') return
  observer = new ResizeObserver(([entry]) => { availableWidth.value = entry.contentRect.width })
  observer.observe(viewport.value)
})
onBeforeUnmount(() => observer?.disconnect())
</script>

<style scoped>
.network-topology-toolbar { display: flex; justify-content: flex-end; align-items: center; flex-wrap: wrap; gap: 10px; margin-bottom: 12px; }
.network-view-controls { display: flex; gap: 4px; }
.network-view-controls button { padding: 6px 9px; border: 1px solid var(--ops-border); border-radius: 5px; background: var(--ops-panel); color: var(--ops-muted); cursor: pointer; font: inherit; font-size: var(--ops-text-xs); }
.network-view-controls button[aria-pressed="true"] { color: var(--ops-accent); border-color: var(--ops-accent-line); background: var(--ops-accent-bg); }
.network-topology-scroll { overflow: auto; max-height: 520px; border: 1px solid var(--ops-border); border-radius: var(--ops-radius-inset); background: var(--ops-inset); }
.network-topology-scroll > svg { display: block; max-width: none; margin-inline: auto; }
.network-column-heading text { fill: var(--ops-muted); font: 12px var(--ops-font-sans); font-weight: var(--ops-weight-semibold); }
.network-arrow { fill: var(--ops-faint); }
.network-edge-line { fill: none; stroke: var(--ops-faint); stroke-width: 1.6; }
.network-node > rect { fill: var(--ops-panel); stroke: var(--ops-border-strong); }
.network-node-copy { display: grid; gap: 3px; }
.network-node-role { color: var(--ops-muted); font-size: 12px; line-height: 1.2; }
.network-node-copy strong { display: -webkit-box; color: var(--ops-text); font-size: 13px; line-height: 1.15; font-weight: var(--ops-weight-semibold); overflow: hidden; overflow-wrap: anywhere; -webkit-line-clamp: 2; -webkit-box-orient: vertical; }
.network-node-meta { color: var(--ops-muted); font-size: 11px; line-height: 1.2; font-variant-numeric: tabular-nums; }
.network-connections { margin-top: 12px; border: 1px solid var(--ops-border); border-radius: var(--ops-radius-inset); padding: 10px 12px; }
.network-connections summary { cursor: pointer; color: var(--ops-muted); font-size: var(--ops-text-sm); }
.network-connections ul { display: grid; gap: 6px; padding-inline-start: 20px; margin: 10px 0 0; overflow-wrap: anywhere; }
.is-compact .network-topology-scroll { max-height: none; }
@media (max-width: 700px) {
  .network-view-controls button { min-height: 44px; }
}
</style>
