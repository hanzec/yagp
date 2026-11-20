// @vitest-environment node
import { afterEach, expect, it, vi } from 'vitest'
import { ElMessage } from 'element-plus'

vi.mock('element-plus', () => ({ ElMessage: { error: vi.fn() } }))

const pageURL = 'https://panel.example.test/?view=ops#/tunnels/42?link=7'

afterEach(() => {
  vi.unstubAllGlobals()
  ElMessage.error.mockClear()
})

it.each([
  // Browsers expose a manual redirect with an opaque status of 0.
  ['a redirect from ForwardAuth', () => Object.defineProperty(new Response(null), 'status', { value: 0 })],
  ['an unauthorized panel response', () => Response.json({ code: 40100 }, { status: 401 })],
  ['an unauthorized proxy response', () => new Response(null, { status: 401 })]
])('opens Authentik once for concurrent requests that receive %s', async (_name, reply) => {
  vi.resetModules()
  const { default: request } = await import('@/utils/request')
  const { getUserInfo } = await import('@/api/auth')
  const { getDashboardStats } = await import('@/api/stats')

  const replace = vi.fn()
  vi.stubGlobal('window', { location: { href: pageURL, replace } })
  const send = vi.fn(async () => reply())
  request.defaults.baseURL = new URL('/api/v1', pageURL).href
  request.defaults.env.fetch = send
  expect(request.defaults.adapter).toBe('fetch')

  const settled = vi.fn()
  getUserInfo().then(settled, settled)
  getDashboardStats().then(settled, settled)

  await vi.waitFor(() => expect(replace).toHaveBeenCalledExactlyOnceWith(
    `/outpost.goauthentik.io/start?${new URLSearchParams({ rd: pageURL })}`
  ))
  expect(send.mock.calls.map(([request]) => request.redirect)).toEqual(['manual', 'manual'])
  expect(ElMessage.error).not.toHaveBeenCalled()
  expect(settled).not.toHaveBeenCalled()
})
