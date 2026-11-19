import { getTunnelList } from '@/api/tunnel'
import { useInventoryList } from '@/utils/inventory-list'

export const connectionsOf = tunnel => tunnel.connections || [tunnel]

export function connectionMatches(connection, entry, exit) {
  return (connection.entry_node_id === entry && connection.exit_node_id === exit) ||
    connection.exit_node_id === entry || connection.entry_node_id === exit
}

export function tunnelPaths(tunnel) {
  const paths = new Map()
  for (const connection of connectionsOf(tunnel)) {
    const id = `${connection.entry_node_id}-${connection.exit_node_id}`
    if (!paths.has(id)) paths.set(id, { ...connection, id })
  }
  return [...paths.values()]
}

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
