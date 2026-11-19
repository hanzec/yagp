import { describe, expect, it } from 'vitest'
import { layoutTunnelTopology } from '@/utils/tunnel-topology'

const names = { 1: 'A 入口', 2: 'B 入口', 3: 'X 中继', 4: 'Y 出口', 5: 'Z 出口' }
const link = (id, entry, exit) => ({
  id, name: `链路 ${id}`, entry_node_id: entry, exit_node_id: exit,
  entry_node: { id: entry, name: names[entry] }, exit_node: { id: exit, name: names[exit] }
})
const network = [link(101, 1, 3), link(102, 2, 3), link(103, 2, 4), link(104, 3, 5)]
const byId = layout => Object.fromEntries(layout.nodes.map(node => [node.id, node]))

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

  it('keeps the same three readable columns in the compact layout', () => {
    const layout = layoutTunnelTopology(network, true)
    const nodes = byId(layout)
    expect(layout.width).toBeLessThanOrEqual(300)
    expect([nodes[1].x, nodes[3].x, nodes[5].x]).toEqual([8, 112, 216])
    expect(nodes[1].x).toBe(nodes[2].x)
    expect(nodes[3].x).toBe(nodes[4].x)
    expect(layout.nodes.every(node => node.x >= 0 && node.y >= 0 && node.x + node.width <= layout.width && node.y + node.height <= layout.height)).toBe(true)
  })
})
