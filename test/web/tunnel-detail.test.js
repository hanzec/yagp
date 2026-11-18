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
const transitive = {
  ...shared, id: 9, name: '上海到新加坡', exit_node_id: 4,
  exit_node: { id: 4, name: '新加坡出口', address: '198.51.100.8' }
}
const onward = { ...tunnel, id: 15, name: '东京到大阪', entry_node_id: 2, entry_node: tunnel.exit_node,
  exit_node_id: 8, exit_node: { id: 8, name: '大阪出口' } }
const parallel = { ...tunnel, id: 11, name: '同节点另一端点' }
const unrelated = {
  ...tunnel, id: 10, name: '独立欧洲链路', entry_node_id: 5, exit_node_id: 6,
  entry_node: { id: 5, name: '柏林入口', address: '192.0.2.5' },
  exit_node: { id: 6, name: '巴黎出口', address: '198.51.100.6' }
}
const inventory = rows => ({ data: { list: rows, total: rows.length } })
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
  getTunnelList.mockResolvedValue(inventory([tunnel]))
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
    const identity = wrapper.get('.tunnel-link-details .tunnel-identity')
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


describe('Tunnel detail connected topology', () => {
  it('shows the selected running links applied Relay protocol and mapped connection address', async () => {
    const selected = {
      ...shared,
      exit_node: { ...shared.exit_node, address_mappings: [
        { listen_ip: '10.10.0.7', connect_ip: '198.51.100.77' },
        { listen_ip: '10.10.0.8', connect_ip: '203.0.113.88' }
      ] },
      exit_rule: {
        id: 42, role: 'exit', name: '待应用出口', protocol: 'quic', listen_ip: '10.10.0.8', listen_port: 9443,
        applied_endpoint: { Protocol: 'wss', ListenIP: '10.10.0.7', ListenPort: 8443 }
      }
    }
    // The current detail replaces the inventory copy before the graph and selection are derived.
    getTunnelList.mockResolvedValue(inventory([tunnel, {
      ...selected, exit_rule: { ...selected.exit_rule, applied_endpoint: undefined }
    }]))
    getTunnel.mockResolvedValue({ data: selected })
    const wrapper = await mountDetail(8)
    expect(wrapper.get('select[aria-label="查看链路"]').element.value).toBe('8')
    const fields = wrapper.findAll('.tunnel-metadata > div')
    const relay = fields.find(field => field.get('dt').text() === 'Relay 监听')
    const connection = fields.find(field => field.get('dt').text() === '入口连接的 Relay 地址')
    expect(relay.get('dd').text()).toBe('WSS · 10.10.0.7:8443')
    expect(connection.get('dd').text()).toBe('198.51.100.77:8443')
    expect(wrapper.get('.tunnel-metadata').text()).not.toContain('QUIC')
    expect(wrapper.get('.tunnel-metadata').text()).not.toContain('203.0.113.88:9443')
    expect(wrapper.get('.tunnel-monitor').text()).toContain(selected.name)
    expect(getTunnelMetrics).toHaveBeenCalledWith('8', '24h', expect.any(AbortSignal))
  })

  it('draws shared entries, shared exits, and an onward link in a single connected network', async () => {
    getTunnelList.mockResolvedValue(inventory([unrelated, shared, tunnel, transitive, parallel, onward]))
    const wrapper = await mountDetail()
    const topology = wrapper.getComponent({ name: 'TunnelTopology' })
    expect(topology.props('tunnels').map(link => link.id).sort((a, b) => a - b)).toEqual([7, 8, 9, 11, 15])
    expect(String(topology.props('selectedId'))).toBe('7')
    expect(topology.findAll('[data-node-id]').map(node => node.attributes('data-node-id')).sort()).toEqual(['1', '2', '3', '4', '8'])
    expect(topology.findAll('[data-link-id]').map(link => link.attributes('data-link-id')).sort()).toEqual(['11', '15', '7', '8', '9'])
    expect(topology.get('[data-link-id="7"]').classes()).toContain('is-selected')
    expect(topology.text()).not.toContain(unrelated.name)
    expect(topology.get('[data-link-id="15"]').attributes('aria-label')).toContain(onward.name)
    const selector = wrapper.get('select[aria-label="查看链路"]')
    expect(selector.findAll('option').map(option => option.element.value).sort()).toEqual(['11', '15', '7', '8', '9'])
    expect(wrapper.get('.tunnel-monitor').text()).toContain(tunnel.name)
  })

  it('waits for every inventory page before drawing the connected network', async () => {
    const firstPage = Array.from({ length: 100 }, (_, index) => ({
      ...unrelated, id: index + 1000, entry_node_id: index * 2 + 100, exit_node_id: index * 2 + 101
    }))
    const lastPage = Promise.withResolvers()
    getTunnelList.mockImplementation(({ page }) => page === 1
      ? Promise.resolve({ data: { list: firstPage, total: 103 } }) : lastPage.promise)
    const wrapper = await mountDetail()
    expect(getTunnelList).toHaveBeenNthCalledWith(1, { page: 1, pageSize: 100 })
    expect(getTunnelList).toHaveBeenNthCalledWith(2, { page: 2, pageSize: 100 })
    expect(wrapper.findComponent({ name: 'TunnelTopology' }).exists()).toBe(false)
    expect(wrapper.find('.tunnel-metrics').exists()).toBe(true)
    lastPage.resolve({ data: { list: [shared, transitive, tunnel], total: 103 } })
    await flushPromises()
    const topology = wrapper.getComponent({ name: 'TunnelTopology' })
    expect(topology.props('tunnels').map(link => link.id).sort((a, b) => a - b)).toEqual([7, 8, 9])
    expect(topology.findAll('[data-node-id]')).toHaveLength(4)
    expect(getTunnelList).toHaveBeenCalledTimes(2)
  })

  it('keeps both parallel links and the reversed link together when opening the return path', async () => {
    const reverse = {
      ...tunnel, id: 12, name: '东京至香港', entry_node_id: tunnel.exit_node_id, exit_node_id: tunnel.entry_node_id,
      entry_node: tunnel.exit_node, exit_node: tunnel.entry_node
    }
    const links = [tunnel, parallel, reverse]
    getTunnelList.mockResolvedValue(inventory(links))
    getTunnel.mockImplementation(id => Promise.resolve({ data: links.find(link => String(link.id) === String(id)) }))
    const wrapper = await mountDetail()
    const graph = wrapper.get('[data-testid="tunnel-topology"]')
    expect(graph.findAll('[data-node-id]')).toHaveLength(2)
    const edges = graph.findAll('[data-link-id]')
    const lines = edges.map(edge => edge.get('path[marker-end]'))
    expect(new Set(lines.map(line => line.attributes('d'))).size).toBe(3)
    await wrapper.vm.$router.push('/tunnels/12')
    await flushPromises()
    expect(graph.findAll('[data-node-id]')).toHaveLength(2)
    expect(graph.get('[data-link-id="12"]').attributes('aria-pressed')).toBe('true')
  })

  it('opens direct member URLs and switches per-link metrics without reloading the network', async () => {
    const rows = [tunnel, shared, transitive, unrelated]
    getTunnelList.mockResolvedValue(inventory(rows))
    getTunnel.mockImplementation(id => Promise.resolve({ data: rows.find(link => String(link.id) === String(id)) }))
    getTunnelMetrics.mockImplementation(id => Promise.resolve({ data: {
      ...metrics, summary: { ...metrics.summary, latest_latency_ms: Number(id) * 10 }
    } }))
    const wrapper = await mountDetail(8)
    const router = wrapper.vm.$router
    expect(getTunnel).toHaveBeenCalledWith('8', expect.any(AbortSignal))
    expect(getTunnelMetrics).toHaveBeenCalledWith('8', '24h', expect.any(AbortSignal))
    expect(wrapper.get('select[aria-label="查看链路"]').element.value).toBe('8')
    expect(wrapper.get('.tunnel-monitor').text()).toContain(shared.name)
    expect(wrapper.get('.tunnel-metrics').text()).toContain('80 ms')
    await wrapper.get('select[aria-label="查看链路"]').setValue('9')
    await flushPromises()
    expect(router.currentRoute.value.params.id).toBe('9')
    expect(getTunnel).toHaveBeenLastCalledWith('9', expect.any(AbortSignal))
    expect(getTunnelMetrics).toHaveBeenLastCalledWith('9', '24h', expect.any(AbortSignal))
    expect(wrapper.get('.tunnel-monitor').text()).toContain(transitive.name)
    expect(wrapper.get('.tunnel-metrics').text()).toContain('90 ms')
    expect(String(wrapper.getComponent({ name: 'TunnelTopology' }).props('selectedId'))).toBe('9')
    expect(getTunnelList).toHaveBeenCalledTimes(1)

    await router.push('/tunnels/10')
    await flushPromises()
    expect(wrapper.getComponent({ name: 'TunnelTopology' }).props('tunnels').map(link => link.id)).toEqual([10])
    expect(wrapper.find('select[aria-label="查看链路"]').exists()).toBe(false)
    expect(wrapper.get('.tunnel-monitor').text()).toContain(unrelated.name)
    expect(wrapper.get('.tunnel-metrics').text()).toContain('100 ms')
    expect(getTunnelList).toHaveBeenCalledTimes(1)
  })

  it('selects an existing link from the graph without changing its configuration', async () => {
    const rows = [tunnel, shared, transitive]
    getTunnelList.mockResolvedValue(inventory(rows))
    getTunnel.mockImplementation(id => Promise.resolve({ data: rows.find(link => String(link.id) === String(id)) }))
    const wrapper = await mountDetail()
    await wrapper.get('[data-link-id="8"]').trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(wrapper.vm.$router.currentRoute.value.params.id).toBe('8')
    expect(wrapper.get('select[aria-label="查看链路"]').element.value).toBe('8')
    expect(getTunnelMetrics).toHaveBeenLastCalledWith('8', '24h', expect.any(AbortSignal))
    expect(getTunnelList).toHaveBeenCalledTimes(1)
  })

  it('ignores an old member response after navigating to a different link', async () => {
    const pendingTunnel = Promise.withResolvers()
    const pendingMetrics = Promise.withResolvers()
    getTunnelList.mockResolvedValue(inventory([tunnel, shared]))
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
    expect(wrapper.get('.tunnel-metrics').text()).toContain('88 ms')
    expect(String(wrapper.getComponent({ name: 'TunnelTopology' }).props('selectedId'))).toBe('8')
  })

  it('keeps metrics usable when the topology fails and retries only the network request', async () => {
    getTunnelList.mockRejectedValueOnce(new Error('inventory unavailable')).mockResolvedValue(inventory([tunnel, shared]))
    const wrapper = await mountDetail()
    const graphError = wrapper.findAll('[role="alert"]').find(alert => alert.text().includes('拓扑'))
    expect(graphError).toBeDefined()
    expect(wrapper.findComponent({ name: 'TunnelTopology' }).exists()).toBe(false)
    expect(wrapper.get('.tunnel-metrics').text()).toContain('50.00 %')
    await wrapper.get('[aria-label="统计时间范围"] button[aria-pressed="false"]').trigger('click')
    await flushPromises()
    expect(getTunnelMetrics).toHaveBeenLastCalledWith('7', '1h', expect.any(AbortSignal))
    const metricsRequests = getTunnelMetrics.mock.calls.length
    await graphError.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.getComponent({ name: 'TunnelTopology' }).props('tunnels').map(link => link.id).sort()).toEqual([7, 8])
    expect(wrapper.findAll('[role="alert"]').some(alert => alert.text().includes('拓扑'))).toBe(false)
    expect(getTunnelList).toHaveBeenCalledTimes(2)
    expect(getTunnel).toHaveBeenCalledTimes(1)
    expect(getTunnelMetrics).toHaveBeenCalledTimes(metricsRequests)
    expect(wrapper.find('.tunnel-metrics').exists()).toBe(true)
  })
})
