import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'
import { getTunnel, getTunnelMetrics } from '@/api/tunnel'
import TunnelDetail from '@/views/TunnelDetail.vue'

vi.mock('@/api/tunnel', () => ({ getTunnel: vi.fn(), getTunnelMetrics: vi.fn() }))

const tunnel = {
  id: 7, name: '示例隧道', status: 'running', protocol: 'wss', relay_port: 8443,
  entry_node: { name: '香港入口', address: '192.0.2.1' }, exit_node: { name: '东京出口', address: '198.51.100.4' }
}
const latest = '2026-11-16T01:03:00Z'
const metrics = {
  from: '2026-11-16T01:00:00Z', to: latest, interval_seconds: 60, bucket_seconds: 60,
  summary: {
    attempts: 3, successful: 1, failed: 1, unavailable: 1,
    min_latency_ms: 0, max_latency_ms: 0, avg_latency_ms: 0, p95_latency_ms: 0,
    latest_latency_ms: 0, latest_ttl: 51, latest_probe_at: latest, latest_status: 'unavailable'
  },
  points: [
    { timestamp: '2026-11-16T01:00:00Z', attempts: 1, successful: 1, failed: 0, unavailable: 0, avg_latency_ms: 0, max_latency_ms: 0, ttl: 51 },
    { timestamp: '2026-11-16T01:01:00Z', attempts: 1, successful: 0, failed: 1, unavailable: 0, avg_latency_ms: null, ttl: null },
    { timestamp: '2026-11-16T01:02:00Z', attempts: 1, successful: 0, failed: 0, unavailable: 1, avg_latency_ms: null, ttl: null }
  ]
}
const empty = {
  ...metrics,
  summary: { attempts: 0, successful: 0, failed: 0, unavailable: 0 },
  points: metrics.points.map(point => ({ timestamp: point.timestamp, attempts: 0 }))
}

enableAutoUnmount(afterEach)

beforeEach(() => {
  vi.resetAllMocks()
  getTunnel.mockResolvedValue({ data: tunnel })
  getTunnelMetrics.mockResolvedValue({ data: metrics })
})

async function mountDetail() {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/tunnels', name: 'Tunnels', component: { template: '<div />' } },
      { path: '/tunnels/:id', name: 'TunnelDetail', component: TunnelDetail, meta: { title: '隧道详情' } }
    ]
  })
  await router.push('/tunnels/7')
  const wrapper = mount(TunnelDetail, { global: { plugins: [router, ElementPlus] } })
  await flushPromises()
  return wrapper
}

describe('Tunnel detail monitoring', () => {
  it('shows topology, nullable readings and the result of the latest attempt separately', async () => {
    const wrapper = await mountDetail()
    expect(getTunnelMetrics).toHaveBeenCalledWith('7', '24h', expect.any(AbortSignal))
    const overviewHeader = wrapper.get('.tunnel-overview > header')
    expect(overviewHeader.get('h2').text()).toBe('隧道信息')
    const identity = overviewHeader.get('.tunnel-identity')
    expect(identity.get('.ops-status').text()).toBe('运行中')
    expect(identity.get('.ops-mono').text()).toBe('#7')
    expect(wrapper.text()).toContain('香港入口')
    expect(wrapper.text()).toContain('东京出口')
    expect(wrapper.text()).toContain('WSS')
    expect(wrapper.text()).toContain('最近探测')
    expect(wrapper.get('.tunnel-caption').text()).toContain('不可用')
    expect(wrapper.get('.tunnel-metrics').text()).toContain('0 ms')
    expect(wrapper.get('.tunnel-metrics').text()).toContain('50.00 %')
    expect(wrapper.get('.tunnel-metrics').text()).toContain('51')
    expect(wrapper.find('.latency-failure-markers').exists()).toBe(true)

    expect(wrapper.find('input[type="range"]').exists()).toBe(false)
    const rows = wrapper.findAll('.latency-chart tbody tr')
    expect(rows).toHaveLength(3)
    expect(rows[0].findAll('td').map(cell => cell.text()).slice(1)).toEqual(['—', '—', '0', '0', '1'])
    expect(rows[1].findAll('td').map(cell => cell.text()).slice(1)).toEqual(['—', '—', '0', '1', '0'])
    expect(rows[2].findAll('td').map(cell => cell.text()).slice(1)).toEqual(['0.0', '51', '1', '0', '0'])
  })

  it.each([
    { successful: 2, failed: 0, rate: '0.00 %' },
    { successful: 2, failed: 1, rate: '33.33 %' },
    { successful: 0, failed: 0, rate: '—' }
  ])('formats failure rate with $successful successful and $failed failed probes', async ({ successful, failed, rate }) => {
    getTunnelMetrics.mockResolvedValue({ data: {
      ...metrics,
      summary: { ...metrics.summary, attempts: successful + failed + 4, successful, failed, unavailable: 4 }
    } })
    const wrapper = await mountDetail()
    const card = wrapper.findAll('.tunnel-metrics > div').find(card => card.get('dt').text() === '失败率')
    expect(card.get('dd').text()).toBe(rate)
    expect(card.get('.tunnel-caption').text()).toBe(`${failed} 次失败 · 4 次不可用`)
  })

  it('ignores stale responses when the time range changes', async () => {
    const pending = Promise.withResolvers()
    getTunnelMetrics.mockReturnValueOnce(pending.promise).mockResolvedValueOnce({ data: empty })
    const wrapper = await mountDetail()
    const oldSignal = getTunnelMetrics.mock.calls[0][2]

    await wrapper.get('button[aria-pressed="false"]').trigger('click')
    await flushPromises()
    expect(oldSignal.aborted).toBe(true)
    expect(getTunnelMetrics).toHaveBeenLastCalledWith('7', '1h', expect.any(AbortSignal))
    expect(wrapper.get('button[aria-pressed="true"]').text()).toBe('1 小时')
    const samplingTable = wrapper.get('.latency-chart table')
    expect(samplingTable.findAll('thead th').map(cell => cell.text())).toEqual(['采样时间', '平均 RTT（ms）', 'TTL', '成功', '失败', '不可用'])
    expect(samplingTable.get('tbody').text()).toBe('此时间范围内暂无探测记录')
    expect(wrapper.find('.latency-plot svg').exists()).toBe(true)
    pending.resolve({ data: metrics })
    await flushPromises()
    expect(samplingTable.get('tbody').text()).toBe('此时间范围内暂无探测记录')
  })

  it('keeps the current chart and table while updating the time range', async () => {
    const pending = Promise.withResolvers()
    getTunnelMetrics.mockResolvedValueOnce({ data: metrics }).mockReturnValueOnce(pending.promise)
    const wrapper = await mountDetail()
    const samplingTable = wrapper.get('.latency-chart table').element

    await wrapper.get('button[aria-pressed="false"]').trigger('click')
    expect(getTunnel).toHaveBeenCalledTimes(1)
    expect(getTunnelMetrics).toHaveBeenLastCalledWith('7', '1h', expect.any(AbortSignal))
    expect(wrapper.get('.tunnel-monitor').attributes('aria-busy')).toBe('true')
    expect(wrapper.get('.latency-chart table').element).toBe(samplingTable)
    expect(wrapper.findAll('.latency-chart tbody tr')).toHaveLength(3)

    pending.resolve({ data: empty })
    await flushPromises()
    expect(wrapper.get('.tunnel-monitor').attributes('aria-busy')).toBe('false')
    expect(wrapper.get('.latency-chart table').element).toBe(samplingTable)
    expect(wrapper.get('.latency-chart tbody').text()).toBe('此时间范围内暂无探测记录')
  })

  it('retries a failed history request', async () => {
    getTunnelMetrics.mockRejectedValueOnce(new Error('API unavailable'))
    const wrapper = await mountDetail()
    expect(wrapper.get('[role="alert"]').text()).toContain('无法加载探测记录')
    await wrapper.get('[role="alert"] button').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.find('.tunnel-metrics').exists()).toBe(true)
  })
})
