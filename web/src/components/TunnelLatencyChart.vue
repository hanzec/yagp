<template>
  <figure class="latency-chart" aria-labelledby="tunnel-latency-title">
    <figcaption class="latency-heading">
      <h3 id="tunnel-latency-title">往返延迟</h3>
      <span class="latency-granularity">{{ bucketLabel }}汇总 · 时间为浏览器本地时间</span>
    </figcaption>

    <div class="latency-legend">
      <span><i class="legend-line" aria-hidden="true"></i>平均 RTT（ms）</span>
      <span v-if="paths.failures"><i class="legend-failure" aria-hidden="true"></i>含失败探测</span>
    </div>
    <div class="latency-plot" aria-hidden="true">
      <div class="latency-ticks ops-mono"><span v-for="n in 5" :key="n">{{ (5 - n) * tickStep }}</span></div>
      <svg viewBox="0 0 1000 200" preserveAspectRatio="none">
        <path class="latency-grid" d="M0 0H1000 M0 50H1000 M0 100H1000 M0 150H1000 M0 200H1000" />
        <path class="latency-line" :d="paths.line" />
        <path class="latency-dots" :d="paths.dots" />
        <path v-if="paths.failures" class="latency-failure-markers" :d="paths.failures" />
      </svg>
    </div>
    <div class="latency-times ops-mono" aria-hidden="true">
      <span v-for="(label, index) in timeTicks" :key="index">{{ label }}</span>
    </div>

    <div class="ops-table-scroll latency-samples" tabindex="0" role="region" aria-label="探测采样记录">
      <table aria-label="采样记录">
        <thead>
          <tr>
            <th scope="col">采样时间</th>
            <th scope="col">平均 RTT（ms）</th>
            <th scope="col">TTL</th>
            <th scope="col">成功</th>
            <th scope="col">失败</th>
            <th scope="col">不可用</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="point in sampledPoints" :key="point.timestamp" class="ops-mono">
            <td><time :datetime="point.timestamp">{{ readingFormat.format(new Date(point.timestamp)) }}</time></td>
            <td>{{ point.avg_latency_ms?.toFixed(1) ?? '—' }}</td>
            <td>{{ point.ttl ?? '—' }}</td>
            <td>{{ point.successful }}</td>
            <td>{{ point.failed }}</td>
            <td>{{ point.unavailable }}</td>
          </tr>
          <tr v-if="!sampledPoints.length">
            <td colspan="6" class="latency-samples-empty">此时间范围内暂无探测记录</td>
          </tr>
        </tbody>
      </table>
    </div>
  </figure>
</template>

<script setup>
import { computed } from 'vue'

const props = defineProps({ metrics: { type: Object, required: true } })
const points = computed(() => props.metrics.points)
const sampledPoints = computed(() => points.value.filter(point => point.attempts).reverse())
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
</script>

<style scoped>
.latency-chart { min-width: 0; margin: 0; padding: 14px; border: 1px solid var(--ops-border); border-radius: var(--ops-radius-inset); background: var(--ops-inset); }
.latency-heading, .latency-legend { display: flex; flex-wrap: wrap; align-items: center; gap: 6px 14px; }
.latency-heading { justify-content: space-between; }
.latency-granularity, .latency-legend, .latency-samples { font-size: var(--ops-text-sm); color: var(--ops-muted); }
.latency-legend { margin-block: 10px; }
.latency-legend span { display: inline-flex; align-items: center; gap: 6px; }
.legend-line { width: 14px; border-top: 2px solid var(--ops-accent); }
.legend-failure { height: 9px; border-left: 2px solid var(--ops-warning); }
.latency-plot { display: grid; grid-template-columns: 4ch minmax(0, 1fr); gap: 8px; }
.latency-plot svg { display: block; width: 100%; height: clamp(136px, 21vw, 256px); overflow: visible; }
.latency-plot path { fill: none; vector-effect: non-scaling-stroke; }
.latency-grid { stroke: var(--ops-border-strong); }
.latency-line, .latency-dots { stroke: var(--ops-accent); stroke-width: 1.75; }
.latency-dots { stroke-width: 2.5; stroke-linecap: round; }
.latency-failure-markers { stroke: var(--ops-warning); stroke-width: 2; }
.latency-ticks, .latency-times { display: flex; justify-content: space-between; color: var(--ops-muted); font-size: var(--ops-text-xs); }
.latency-ticks { flex-direction: column; align-items: end; margin-block: -8px; }
.latency-times { padding: 8px 0 0 calc(4ch + 12px); }
.latency-samples { max-height: 14rem; margin-top: 12px; overflow: auto; }
.latency-samples table { width: 100%; border-collapse: collapse; text-align: start; }
.latency-samples :is(th, td) { padding: 6px 8px; border-bottom: 1px solid var(--ops-border); }
.latency-samples :is(th, td):not(:first-child) { text-align: end; }
.latency-samples th { position: sticky; top: 0; background: var(--ops-inset); }
.latency-samples .latency-samples-empty { padding-block: 16px; text-align: center; }
</style>
