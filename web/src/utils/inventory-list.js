import { ref } from 'vue'

export function useInventoryList(loadPage) {
  const items = ref([])
  const total = ref(null)
  const loading = ref(true)
  const loadError = ref(false)
  let activeRequest = null

  async function fetchData(silent = false) {
    if (silent && (activeRequest || loadError.value)) return
    // A user action supersedes an older request. A polling tick waits for it to finish.
    const request = Symbol()
    activeRequest = request
    if (!silent) loading.value = true
    try {
      const { data } = await loadPage()
      if (activeRequest !== request) return
      items.value = data.list || []
      total.value = data.total || 0
      loadError.value = false
    } catch {
      if (activeRequest === request) loadError.value = true
    } finally {
      if (activeRequest === request) {
        activeRequest = null
        loading.value = false
      }
    }
  }
  return { items, total, loading, loadError, fetchData }
}
