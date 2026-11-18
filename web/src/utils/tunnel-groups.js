import { getTunnelList } from '@/api/tunnel'
import { useInventoryList } from '@/utils/inventory-list'

export function useTunnelInventory() {
  return useInventoryList(async () => {
    const rows = []
    const size = 100
    for (let page = 1; ; page++) {
      const { data } = await getTunnelList({ page, pageSize: size })
      const batch = data.list || []
      rows.push(...batch)
      if (rows.length >= data.total || batch.length < size) break
    }
    return { data: { list: rows, total: rows.length } }
  })
}

// Group the complete inventory before filtering or paging: any shared physical node connects links.
export function groupTunnels(tunnels) {
  const parent = new Map()
  const root = id => {
    if (!parent.has(id)) parent.set(id, id)
    if (parent.get(id) !== id) parent.set(id, root(parent.get(id)))
    return parent.get(id)
  }
  const nodesOf = tunnel => [...new Set([tunnel.entry_node_id, tunnel.exit_node_id].filter(id => id != null && id !== 0).map(String))]
  for (const tunnel of tunnels) {
    const [first, ...others] = nodesOf(tunnel)
    if (first === undefined) continue
    for (const node of others) parent.set(root(node), root(first))
  }
  const groups = new Map()
  for (const tunnel of tunnels) {
    const ids = nodesOf(tunnel)
    const key = ids.length ? root(ids[0]) : `tunnel-${tunnel.id}`
    if (!groups.has(key)) groups.set(key, { id: tunnel.id, tunnels: [], nodeIds: [] })
    const group = groups.get(key)
    group.id = Math.min(group.id, tunnel.id)
    group.tunnels.push(tunnel)
    group.nodeIds = [...new Set([...group.nodeIds, ...ids])]
  }
  return [...groups.values()]
}

export function connectedGroups(groups, nodeIds) {
  const selected = nodeIds.filter(id => id != null).map(String)
  return groups.filter(group => group.nodeIds.some(id => selected.includes(id)))
}
