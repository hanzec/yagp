import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import TunnelTopology from '@/components/TunnelTopology.vue'
import { layoutTunnelTopology } from '@/utils/tunnel-topology'

const names = { 1: 'A 入口', 2: 'B 入口', 3: 'X 中继', 4: 'Y 出口', 5: 'Z 出口' }
const link = (id, entry, exit) => ({
  id, name: `链路 ${id}`, entry_node_id: entry, exit_node_id: exit,
  entry_node: { id: entry, name: names[entry] }, exit_node: { id: exit, name: names[exit] }
})
const network = [link(101, 1, 3), link(102, 2, 3), link(103, 2, 4), link(104, 3, 5)]
const byId = layout => Object.fromEntries(layout.nodes.map(node => [node.id, node]))

enableAutoUnmount(afterEach)
afterEach(() => vi.unstubAllGlobals())

describe('Physical tunnel topology', () => {
  it('aligns parallel nodes and renders a shared entry/exit as one intermediate node', () => {
    const layout = layoutTunnelTopology(network)
    const nodes = byId(layout)
    expect(layout.nodes).toHaveLength(5)
    expect([nodes[1].column, nodes[2].column]).toEqual([0, 0])
    expect([nodes[3].column, nodes[4].column, nodes[5].column]).toEqual([1, 1, 2])
    expect(nodes[1].x).toBe(nodes[2].x)
    expect(nodes[3].x).toBe(nodes[4].x)
    expect(nodes[5].x).toBeGreaterThan(nodes[3].x)
    expect(nodes[5].y).toBe(nodes[3].y)
    expect(layout.links[0].target).toBe(layout.links[3].source)
    expect(layout.columns.map(column => column.title)).toEqual(['始发入口', '中继 / 出口', '终点出口'])
  })

  it('uses the longest preceding path when a later edge also reaches a node directly', () => {
    const nodes = byId(layoutTunnelTopology([link(1, 1, 5), link(2, 1, 3), link(3, 3, 4), link(4, 4, 5)]))
    expect([nodes[1].column, nodes[3].column, nodes[4].column, nodes[5].column]).toEqual([0, 1, 2, 3])
  })

  it('aligns short and long source branches immediately before their shared destination', () => {
    const nodes = byId(layoutTunnelTopology([link(1, 5, 2), link(2, 1, 3), link(3, 2, 3)]))
    expect([nodes[5].column, nodes[1].column, nodes[2].column, nodes[3].column]).toEqual([0, 1, 1, 2])
  })

  it('keeps cycles in one layer with distinct directed curves and preserves their incoming/outgoing layers', () => {
    const layout = layoutTunnelTopology([link(1, 1, 3), link(2, 3, 4), link(3, 4, 3), link(4, 4, 5)])
    const nodes = byId(layout)
    expect(layout.nodes).toHaveLength(4)
    expect([nodes[1].column, nodes[3].column, nodes[4].column, nodes[5].column]).toEqual([0, 1, 1, 2])
    expect(new Set(layout.links.map(edge => edge.path)).size).toBe(4)
    expect(layout.columns[1].title).toBe('循环连接')
    expect(() => JSON.stringify(layout)).not.toThrow()
  })

  it('keeps parallel connections selectable separately and reports the pair count', () => {
    const layout = layoutTunnelTopology([link(1, 1, 3), link(2, 1, 3), link(3, 2, 3)])
    expect(layout.nodes).toHaveLength(3)
    expect(layout.links.map(edge => edge.parallelCount)).toEqual([2, 2, 1])
    expect(layout.links.map(edge => edge.parallelIndex)).toEqual([1, 2, 1])
    expect(new Set(layout.links.map(edge => edge.path)).size).toBe(3)
    expect(layout.links[1].description).toContain('第 2/2 条链路')
  })

  it('keeps the same three readable columns in the compact layout', () => {
    const layout = layoutTunnelTopology(network, true)
    const nodes = byId(layout)
    expect(layout.width).toBeLessThanOrEqual(300)
    expect([nodes[1].x, nodes[3].x, nodes[5].x]).toEqual([8, 112, 216])
    expect(nodes[1].x).toBe(nodes[2].x)
    expect(nodes[3].x).toBe(nodes[4].x)
    expect(layout.nodes.every(node => node.x >= 0 && node.y >= 0 && node.x + node.width <= layout.width && node.y + node.height <= layout.height)).toBe(true)
  })

  it('separates labels at crossings and fills a node name reported by a later connection', () => {
    const layout = layoutTunnelTopology([link(1, 1, 3), link(2, 1, 4), link(3, 2, 3), link(4, 2, 4)])
    for (let left = 0; left < layout.links.length; left++) for (let right = left + 1; right < layout.links.length; right++) {
      const a = layout.links[left], b = layout.links[right]
      expect(Math.abs(a.labelX - b.labelX) >= layout.labelWidth || Math.abs(a.labelY - b.labelY) >= layout.labelHeight + 6).toBe(true)
    }
    const unnamed = { ...link(1, 1, 3), entry_node: undefined }
    expect(byId(layoutTunnelTopology([unnamed, link(2, 3, 1)]))[1].name).toBe('A 入口')
  })

  it('selects actual links by pointer or keyboard without moving the focused edge in the DOM', async () => {
    const wrapper = mount(TunnelTopology, { props: { tunnels: network, selectedId: 101 }, attachTo: document.body })
    expect(wrapper.findAll('[data-node-id="3"]')).toHaveLength(1)
    expect(wrapper.findAll('[data-link-id]')).toHaveLength(4)
    const edge = wrapper.get('[data-link-id="104"]')
    expect(edge.attributes('aria-label')).toContain('X 中继 → Z 出口')
    edge.element.focus()
    await edge.trigger('keydown', { key: 'Enter' })
    expect(wrapper.emitted('select')).toContainEqual([104])
    await wrapper.setProps({ selectedId: 104 })
    expect(document.activeElement).toBe(edge.element)
    expect(edge.classes()).toContain('is-selected')
    expect(wrapper.findAll('[data-link-id]').map(item => item.attributes('data-link-id'))).toEqual(['101', '102', '103', '104'])
    await wrapper.get('[data-link-id="102"]').trigger('click')
    expect(wrapper.emitted('select')).toContainEqual([102])
  })

  it('keeps left-to-right compact layers, exposes original size and readable connection descriptions', async () => {
    vi.stubGlobal('ResizeObserver', class {
      constructor(callback) { this.callback = callback }
      observe() { this.callback([{ contentRect: { width: 220 } }]) }
      disconnect() {}
    })
    const wrapper = mount(TunnelTopology, { props: { tunnels: network, selectedId: 101 } })
    await flushPromises()
    expect(wrapper.classes()).toContain('is-compact')
    expect(wrapper.get('[data-node-id="1"]').attributes('data-node-column')).toBe('0')
    expect(wrapper.get('[data-node-id="3"]').attributes('data-node-column')).toBe('1')
    expect(wrapper.get('[data-node-id="5"]').attributes('data-node-column')).toBe('2')
    const graph = wrapper.get('svg[aria-label="隧道节点与有向链路"]')
    expect(graph.attributes('style')).toContain('width: 220px')
    expect(wrapper.get('details').attributes('open')).toBeDefined()
    await wrapper.get('[aria-label="按原始大小查看拓扑图"]').trigger('click')
    expect(graph.attributes('style')).toContain('width: 288px')
    expect(wrapper.get('[data-link-id="101"] .network-edge-label').text()).toBe('#101')
    expect(wrapper.get('[data-link-id="101"]').attributes('aria-label')).toContain('A 入口 → X 中继')
    expect(wrapper.get('.network-connections').text()).toContain('X 中继 → Z 出口')
  })
})
