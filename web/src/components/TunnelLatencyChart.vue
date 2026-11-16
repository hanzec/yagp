<template>
  <figure class="latency-chart" aria-labelledby="tunnel-latency-title">
    <figcaption class="latency-heading">
      <h3 id="tunnel-latency-title">往返延迟</h3>
      <span class="latency-granularity">{{ bucketLabel }}汇总 · 时间为浏览器本地时间</span>
    </figcaption>

    <template v-if="hasLatency">
      <div class="latency-legend">
        <span><i class="legend-line" aria-hidden="true"></i>平均 RTT（ms）</span>
        <span v-if="hasFailures"><i class="legend-failure" aria-hidden="true"></i>含失败探测</span>
      </div>
      <div class="latency-plot" aria-hidden="true">
        <div class="latency-ticks ops-mono"><span v-for="n in 5" :key="n">{{ (5 - n) * tickStep }}</span></div>
        <svg viewBox="0 0 1000 200" preserveAspectRatio="none" @pointermove="selectOnPlot" @pointerdown="selectOnPlot">
          <path class="latency-grid" d="M0 0H1000 M0 50H1000 M0 100H1000 M0 150H1000 M0 200H1000" />
          <path v-if="selected" class="latency-cursor" :d="`M${xAt(selected.timestamp)} 0V200`" />
          <path class="latency-line" :d="paths.line" />
          <path class="latency-dots" :d="paths.dots" />
          <path v-if="paths.failures" class="latency-failure-markers" :d="paths.failures" />
        </svg>
      </div>
      <div class="latency-times ops-mono" aria-hidden="true">
        <span v-for="(label, index) in timeTicks" :key="index">{{ label }}</span>
      </div>
    </template>
    <p v-else class="latency-empty">此时间范围内暂无可绘制的 RTT。</p>

    <div v-if="selected" class="latency-inspector">
      <label for="tunnel-probe-bucket">查看采样时段</label>
      <input
        id="tunnel-probe-bucket"
        v-model.number="selection"
        type="range"
        min="0"
        :max="points.length - 1"
        :disabled="points.length < 2"
        :aria-valuetext="selectionText"
      />
      <div id="tunnel-probe-reading" class="latency-reading">
        <time class="ops-mono" :datetime="selected.timestamp">{{ readingTime }}</time>
        <strong>{{ selected.avg_latency_ms == null ? '—' : selected.avg_latency_ms.toFixed(1) + ' ms' }}</strong>
        <span v-if="selected.ttl != null">TTL {{ selected.ttl }}</span>
        <span v-if="selected.attempts">{{ selected.successful }} 成功 · {{ selected.failed }} 失败 · {{ selected.unavailable }} 不可用</span>
        <span v-else>无探测记录</span>
      </div>
    </div>
  </figure>
</template>

<script setup>
import { computed, ref } from 'vue'

const props = defineProps({ metrics: { type: Object, required: true } })
const selectedIndex = ref(null)
const points = computed(() => props.metrics.points)
const hasLatency = computed(() => points.value.some(point => point.avg_latency_ms != null))
const hasFailures = computed(() => points.value.some(point => point.failed))
const bucketSeconds = computed(() => props.metrics.bucket_seconds)
const bucketLabel = computed(() => bucketSeconds.value >= 3600 ? `${bucketSeconds.value / 3600} 小时` : `${bucketSeconds.value / 60} 分钟`)
const windowStart = computed(() => Date.parse(props.metrics.from))
const windowLength = computed(() => Date.parse(props.metrics.to) - windowStart.value)
const xAt = timestamp => Math.max(0, Math.min(1000, 1000 * (Date.parse(timestamp) - windowStart.value) / windowLength.value))

const tickStep = computed(() => {
  const peak = Math.max(0, ...points.value.map(point => point.max_latency_ms ?? point.avg_latency_ms ?? 0))
  const raw = Math.max(1, peak / 4)
  const magnitude = 10 ** Math.floor(Math.log10(raw))
  return [1, 2, 5, 10].find(step => step * magnitude >= raw) * magnitude
})

const paths = computed(() => {
  const line = []
  const dots = []
  const failures = []
  let adjacent = false
  for (const point of points.value) {
    const x = xAt(point.timestamp)
    if (point.failed) failures.push(`M${x} 193V199`)
    if (point.avg_latency_ms == null) {
      adjacent = false
      continue
    }
    const y = Math.max(1, Math.min(199, 200 - 50 * point.avg_latency_ms / tickStep.value))
    line.push(`${adjacent ? 'L' : 'M'}${x} ${y}`)
    dots.push(`M${x} ${y}v.01`)
    adjacent = true
  }
  return { line: line.join(' '), dots: dots.join(' '), failures: failures.join(' ') }
})

const selection = computed({
  get: () => Math.min(selectedIndex.value ?? Math.max(0, points.value.findLastIndex(point => point.attempts)), Math.max(0, points.value.length - 1)),
  set: index => { selectedIndex.value = index }
})
const selected = computed(() => points.value[selection.value])
const day = 86400000
const hourFormat = new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit' })
const dateFormat = new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit' })
const readingFormat = new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
const timeTicks = computed(() => {
  return [0, 0.5, 1].map(fraction => {
    const time = windowStart.value + fraction * windowLength.value
    if (windowLength.value > day) return dateFormat.format(time)
    if (windowLength.value === day && fraction !== 0.5) return readingFormat.format(time)
    return hourFormat.format(time)
  })
})
const readingTime = computed(() => readingFormat.format(new Date(selected.value.timestamp)))
const selectionText = computed(() => {
  const point = selected.value
  const latency = point.avg_latency_ms == null ? '无延迟读数' : `${point.avg_latency_ms.toFixed(1)} 毫秒`
  const ttl = point.ttl == null ? '' : `，TTL ${point.ttl}`
  return `${readingTime.value}，${latency}${ttl}，${point.successful} 成功，${point.failed} 失败，${point.unavailable} 不可用`
})

function selectOnPlot(event) {
  if (event.type === 'pointermove' && event.pointerType !== 'mouse') return
  const rect = event.currentTarget.getBoundingClientRect()
  const x = 1000 * (event.clientX - rect.left) / rect.width
  selection.value = points.value.reduce((closest, point, index) =>
    Math.abs(xAt(point.timestamp) - x) < Math.abs(xAt(points.value[closest].timestamp) - x) ? index : closest, 0)
}
</script>

<style scoped>
.latency-chart { min-width: 0; margin: 0; padding: 14px; border: 1px solid var(--ops-border); border-radius: var(--ops-radius-inset); background: var(--ops-inset); }
.latency-heading, .latency-legend, .latency-reading { display: flex; flex-wrap: wrap; align-items: center; gap: 6px 14px; }
.latency-heading { justify-content: space-between; }
.latency-granularity, .latency-legend, .latency-reading, .latency-empty { font-size: var(--ops-text-sm); color: var(--ops-muted); }
.latency-legend { margin-block: 10px; }
.latency-legend span { display: inline-flex; align-items: center; gap: 6px; }
.legend-line { width: 14px; border-top: 2px solid var(--ops-accent); }
.legend-failure { height: 9px; border-left: 2px solid var(--ops-warning); }
.latency-plot { display: grid; grid-template-columns: 4ch minmax(0, 1fr); gap: 8px; }
.latency-plot svg { display: block; width: 100%; height: clamp(136px, 21vw, 256px); overflow: visible; cursor: crosshair; }
.latency-plot path { fill: none; vector-effect: non-scaling-stroke; }
.latency-grid { stroke: var(--ops-border-strong); }
.latency-line, .latency-dots { stroke: var(--ops-accent); stroke-width: 1.75; }
.latency-dots { stroke-width: 2.5; stroke-linecap: round; }
.latency-cursor { stroke: var(--ops-muted); stroke-dasharray: 3 4; }
.latency-failure-markers { stroke: var(--ops-warning); stroke-width: 2; }
.latency-ticks, .latency-times { display: flex; justify-content: space-between; color: var(--ops-muted); font-size: var(--ops-text-xs); }
.latency-ticks { flex-direction: column; align-items: end; margin-block: -8px; }
.latency-times { padding: 8px 0 0 calc(4ch + 12px); }
.latency-empty { padding: 16px 0; }
.latency-inspector { display: grid; align-items: center; grid-template-columns: auto minmax(0, 1fr); column-gap: 12px; margin-top: 8px; }
.latency-inspector label { color: var(--ops-muted); font-size: var(--ops-text-sm); }
.latency-inspector input { min-width: 0; min-height: 44px; margin: 0; accent-color: var(--ops-accent); }
.latency-reading { grid-column: 1 / -1; min-width: 0; padding-top: 6px; border-top: 1px solid var(--ops-border); }
.latency-reading strong { color: var(--ops-text); font-variant-numeric: tabular-nums; }
</style>
