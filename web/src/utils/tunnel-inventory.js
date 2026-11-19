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
