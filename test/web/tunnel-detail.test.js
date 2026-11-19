import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'
import { getTunnel, getTunnelList, getTunnelMetrics } from '@/api/tunnel'
import TunnelDetail from '@/views/TunnelDetail.vue'

vi.mock('@/api/tunnel', () => ({ getTunnel: vi.fn(), getTunnelList: vi.fn(), getTunnelMetrics: vi.fn() }))

const tunnel = {
  id: 7, name: '示例隧道', status: 'running', protocol: 'wss', relay_port: 8443,
  entry_node_id: 1, exit_node_id: 2,
  entry_node: { id: 1, name: '香港入口', address: '192.0.2.1' }, exit_node: { id: 2, name: '东京出口', address: '198.51.100.4' }
}
const shared = {
  ...tunnel, id: 8, name: '备用入口链路', entry_node_id: 3,
  entry_node: { id: 3, name: '上海入口', address: '192.0.2.3' }
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

async function mountDetail(id = 7) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/tunnels', name: 'Tunnels', component: { template: '<div />' } },
      { path: '/tunnels/:id', name: 'TunnelDetail', component: TunnelDetail, meta: { title: '隧道详情' } }
    ]
  })
  await router.push('/tunnels/' + id)
  const wrapper = mount(TunnelDetail, { global: { plugins: [router] } })
  await flushPromises()
  return wrapper
}

describe('Tunnel detail monitoring', () => {
  it('shows topology, nullable readings and the result of the latest attempt separately', async () => {
    const wrapper = await mountDetail()
    expect(getTunnelMetrics).toHaveBeenCalledWith('7', '24h', expect.any(AbortSignal))
    const overviewHeader = wrapper.get('.tunnel-overview > header')
    expect(overviewHeader.get('h2').text()).toBe('隧道拓扑')
    const identity = wrapper.get('.tunnel-overview .tunnel-identity')
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
    const chart = wrapper.get('.latency-plot svg')
    expect(chart.get('.latency-line').attributes('d')).toBeTruthy()
    expect(chart.find('.latency-failure-markers').exists()).toBe(true)
    expect(wrapper.find('.latency-chart table').exists()).toBe(false)
    expect(wrapper.find('input[type="range"]').exists()).toBe(false)
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

    await wrapper.get('[aria-label="统计时间范围"] button[aria-pressed="false"]').trigger('click')
    await flushPromises()
    expect(oldSignal.aborted).toBe(true)
    expect(getTunnelMetrics).toHaveBeenLastCalledWith('7', '1h', expect.any(AbortSignal))
    expect(wrapper.get('[aria-label="统计时间范围"] button[aria-pressed="true"]').text()).toBe('1 小时')
    const chart = wrapper.get('.latency-plot svg')
    expect(chart.get('.latency-grid').attributes('d')).toBeTruthy()
    expect(chart.get('.latency-line').attributes('d')).toBe('')
    expect(chart.find('.latency-failure-markers').exists()).toBe(false)
    expect(wrapper.find('.latency-chart table').exists()).toBe(false)
    pending.resolve({ data: metrics })
    await flushPromises()
    expect(chart.get('.latency-line').attributes('d')).toBe('')
  })

  it('keeps the current chart mounted while updating the time range', async () => {
    const pending = Promise.withResolvers()
    getTunnelMetrics.mockResolvedValueOnce({ data: metrics }).mockReturnValueOnce(pending.promise)
    const wrapper = await mountDetail()
    const chart = wrapper.get('.latency-plot svg')
    const line = chart.get('.latency-line').attributes('d')

    await wrapper.get('[aria-label="统计时间范围"] button[aria-pressed="false"]').trigger('click')
    expect(getTunnel).toHaveBeenCalledTimes(1)
    expect(getTunnelMetrics).toHaveBeenLastCalledWith('7', '1h', expect.any(AbortSignal))
    expect(wrapper.get('.tunnel-monitor').attributes('aria-busy')).toBe('true')
    expect(wrapper.get('.latency-plot svg').element).toBe(chart.element)
    expect(chart.get('.latency-line').attributes('d')).toBe(line)

    pending.resolve({ data: empty })
    await flushPromises()
    expect(wrapper.get('.tunnel-monitor').attributes('aria-busy')).toBe('false')
    expect(wrapper.get('.latency-plot svg').element).toBe(chart.element)
    expect(chart.get('.latency-grid').attributes('d')).toBeTruthy()
    expect(chart.get('.latency-line').attributes('d')).toBe('')
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


describe('Managed tunnel details', () => {
  it('shows a single node pair and chart, with all endpoint configurations visible together', async () => {
    const group = {
      ...tunnel,
      exit_node: { ...tunnel.exit_node, address_mappings: [
        { listen_ip: '10.10.0.7', connect_ip: '198.51.100.77' },
        { listen_ip: '10.10.0.8', connect_ip: '203.0.113.88' }
      ] },
      connections: [
        { id: 70, status: 'running', entry_rule: { id: 42, name: '网站入口', listen_port: 8888 },
          exit_rule: { id: 43, role: 'exit', name: '网站出口', protocol: 'quic', listen_ip: '10.10.0.8',
            listen_port: 9443, applied_endpoint: { Protocol: 'wss', ListenIP: '10.10.0.7', ListenPort: 8443 } } },
        { id: 71, status: 'running', entry_rule: { id: 44, name: '仓库入口', listen_port: 9888 },
          exit_rule: { id: 45, role: 'exit', name: '仓库出口', protocol: 'mtls', listen_ip: '10.10.0.8', listen_port: 10001 } }
      ]
    }
    getTunnel.mockResolvedValue({ data: group })
    const wrapper = await mountDetail()
    const direction = wrapper.get('[aria-label="转发方向"]')
    expect(direction.findAll('dd').map(item => item.text())).toEqual(['香港入口', '东京出口'])
    expect(wrapper.get('.tunnel-overview .ops-mono').text()).toBe('#7')
    const [first, second] = wrapper.findAll('.tunnel-metadata')
    expect(first.text()).toContain('网站入口')
    expect(first.text()).toContain('WSS · 10.10.0.7:8443')
    expect(first.text()).toContain('198.51.100.77:8443')
    expect(first.text()).not.toContain('QUIC')
    expect(second.text()).toContain('仓库入口')
    expect(second.text()).toContain('MTLS · 10.10.0.8:10001')
    expect(second.text()).toContain('203.0.113.88:10001')
    expect(wrapper.find('select[aria-label="查看链路"]').exists()).toBe(false)
    expect(wrapper.findAll('.latency-plot')).toHaveLength(1)
    expect(wrapper.get('.tunnel-caption').text()).toContain('汇总全部端点的探测结果')
    expect(getTunnelList).not.toHaveBeenCalled()
    expect(getTunnelMetrics).toHaveBeenCalledExactlyOnceWith('7', '24h', expect.any(AbortSignal))
  })

  it('opens an old connection URL at the managed tunnel ID', async () => {
    const wrapper = await mountDetail(70)
    expect(wrapper.vm.$router.currentRoute.value.params.id).toBe('7')
    expect(getTunnel).toHaveBeenNthCalledWith(1, '70', expect.any(AbortSignal))
    expect(getTunnelMetrics).toHaveBeenLastCalledWith('7', '24h', expect.any(AbortSignal))
    expect(wrapper.get('.tunnel-overview .ops-mono').text()).toBe('#7')
  })

  it('ignores an old response after navigating to a different pair', async () => {
    const pendingTunnel = Promise.withResolvers()
    const pendingMetrics = Promise.withResolvers()
    getTunnel.mockReturnValueOnce(pendingTunnel.promise).mockResolvedValueOnce({ data: shared })
    getTunnelMetrics.mockReturnValueOnce(pendingMetrics.promise).mockResolvedValueOnce({ data: {
      ...metrics, summary: { ...metrics.summary, latest_latency_ms: 88 }
    } })
    const wrapper = await mountDetail()
    const oldTunnelSignal = getTunnel.mock.calls[0][1]
    const oldMetricsSignal = getTunnelMetrics.mock.calls[0][2]
    await wrapper.vm.$router.push('/tunnels/8')
    await flushPromises()
    expect(oldTunnelSignal.aborted).toBe(true)
    expect(oldMetricsSignal.aborted).toBe(true)
    expect(wrapper.get('.tunnel-detail-name').text()).toBe(shared.name)
    expect(wrapper.get('.tunnel-metrics').text()).toContain('88 ms')
    pendingTunnel.resolve({ data: tunnel })
    pendingMetrics.resolve({ data: metrics })
    await flushPromises()
    expect(wrapper.get('.tunnel-detail-name').text()).toBe(shared.name)
    expect(wrapper.get('.tunnel-overview .ops-mono').text()).toBe('#8')
    expect(wrapper.get('.tunnel-metrics').text()).toContain('88 ms')
  })
})
