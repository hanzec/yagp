import { flushPromises } from '@vue/test-utils'
import { ElSwitch } from 'element-plus'
import { nextTick } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import NodeGostService from '@/components/NodeGostService.vue'
import NodeNftPreview from '@/components/NodeNftPreview.vue'
import NodeUpgradeDialog from '@/components/NodeUpgradeDialog.vue'
import NodeUpgradePanel from '@/components/NodeUpgradePanel.vue'
import * as api from '@/api/node'
import { apiError, button, click, deferred, mockNodeMessages, mountNodeComponent, response } from './node-test-support.js'

vi.mock('@/api/node', () => ({
  getGostService: vi.fn(),
  getGostLogs: vi.fn(),
  restartGostService: vi.fn(),
  getNodeNftConfig: vi.fn(),
  getNodeFirewallStatus: vi.fn(),
  enableNodeFirewall: vi.fn(),
  getNodeUpgradeStatus: vi.fn(),
  updateNodeUpgradePolicy: vi.fn(),
  upgradeNodeComponent: vi.fn()
}))

const node = { id: 7, name: '边缘节点' }
let wrapper
let messages

beforeEach(() => {
  vi.resetAllMocks()
  messages = mockNodeMessages()
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.restoreAllMocks()
  vi.useRealTimers()
})

function open(component, props = {}) {
  wrapper = mountNodeComponent(component, { node, modelValue: true, ...props })
  return wrapper
}

describe('inline node service status', () => {
  const active = { unit: 'gost.service', active_state: 'active', load_state: 'loaded', sub_state: 'running' }
  const statusFields = view => Object.fromEntries(view.findAll('dl dt')
    .map(label => [label.text(), label.element.nextElementSibling.textContent.trim()]))
  const mountService = (status = 'online') => {
    wrapper = mountNodeComponent(NodeGostService, { node: { ...node, status } })
    return wrapper
  }

  beforeEach(() => {
    api.getNodeFirewallStatus.mockResolvedValue(response({ backend: 'nftables', supported: true, managed: false }))
  })

  it('loads status in the page and sorts service logs when expanded', async () => {
    const status = deferred()
    api.getGostService.mockReturnValueOnce(status.promise)
    api.getGostLogs.mockResolvedValueOnce(response({ entries: [
      { message: 'older', priority: 6, timestamp_unix_micros: 1_000_000 },
      { message: 'newer', priority: 42, timestamp_unix_micros: 2_000_000 },
      { message: 'unknown', timestamp_unix_micros: 0 }
    ] }))
    const view = mountService()
    expect(view.get('#node-gost-service h2').text()).toBe('服务状态')
    expect(view.findAll('h3').map(heading => heading.text())).toEqual(['Gost', '自动防火墙规则'])
    expect(view.find('[role="dialog"]').exists()).toBe(false)
    expect(view.text()).toContain('正在读取服务状态')
    expect(button(view, '重启 Gost').element.disabled).toBe(true)
    status.resolve(response(active))
    await flushPromises()
    expect(api.getGostService).toHaveBeenCalledWith(node.id)
    expect(api.getNodeFirewallStatus).toHaveBeenCalledExactlyOnceWith(node.id)
    expect(api.getGostLogs).not.toHaveBeenCalled()
    expect(view.text()).toContain('运行中')
    const gost = view.get('[aria-labelledby="gost-status-title"]')
    expect(statusFields(gost).后端类型).toBe('systemd')
    const logToggle = view.get('button[aria-controls="node-gost-logs"]')
    expect(logToggle.attributes('aria-expanded')).toBe('false')
    expect(view.get('#node-gost-logs').isVisible()).toBe(false)
    await logToggle.trigger('click')
    await flushPromises()
    expect(logToggle.attributes('aria-expanded')).toBe('true')
    expect(api.getGostLogs).toHaveBeenCalledWith(node.id)
    expect(view.findAll('.gost-log-message').map(item => item.text())).toEqual(['newer', 'older', 'unknown'])
    expect(view.text()).toContain('未知级别')
    expect(view.text()).toContain('未知时间')
    expect(view.text()).toContain('紧急')

    api.getGostService.mockResolvedValue(response({ ...active, active_state: 'activating', sub_state: '' }))
    api.getGostLogs.mockResolvedValue(response({}))
    await click(view, '刷新')
    expect(api.getNodeFirewallStatus.mock.calls).toEqual([[node.id], [node.id]])
    expect(view.get('.ops-status').classes()).toContain('is-warning')
    expect(view.text()).toContain('未知')
    expect(view.text()).toContain('暂无日志')
    await logToggle.trigger('click')
    expect(logToggle.attributes('aria-expanded')).toBe('false')
    expect(view.get('#node-gost-logs').isVisible()).toBe(false)
  })

  it('retries independent read failures and handles restart cancellation, failure and success', async () => {
    api.getGostService.mockRejectedValueOnce(apiError('节点离线')).mockResolvedValue(response(active))
    api.getGostLogs.mockRejectedValueOnce(new Error('offline')).mockResolvedValue(response({ entries: [] }))
    const view = mountService()
    await view.get('button[aria-controls="node-gost-logs"]').trigger('click')
    await flushPromises()
    expect(view.text()).toContain('节点离线')
    expect(view.text()).toContain('无法读取 Gost 日志')
    await click(view, '刷新')
    expect(view.findAll('[role="alert"]')).toHaveLength(0)

    messages.confirm.mockRejectedValueOnce('cancel')
    await click(view, '重启 Gost')
    expect(api.restartGostService).not.toHaveBeenCalled()

    api.restartGostService.mockRejectedValueOnce(apiError('缺少重启权限'))
    await click(view, '重启 Gost')
    expect(view.get('[role="alert"]').text()).toBe('缺少重启权限')
    api.restartGostService.mockRejectedValueOnce(new Error('lost'))
    await click(view, '重启 Gost')
    expect(view.get('[role="alert"]').text()).toContain('重启请求失败')
    api.restartGostService.mockResolvedValueOnce(response(active))
    await click(view, '重启 Gost')
    expect(api.restartGostService).toHaveBeenCalledWith(node.id)
    expect(api.getNodeFirewallStatus).toHaveBeenCalledTimes(5)
    expect(api.getGostLogs).toHaveBeenCalledTimes(5)
    expect(messages.success).toHaveBeenCalledWith('Gost 重启操作已完成')
    expect(view.find('[role="alert"]').exists()).toBe(false)
  })

  it('avoids auto queries for offline nodes and supports manual refresh and recovery', async () => {
    api.getGostService.mockRejectedValueOnce(new Error('offline')).mockResolvedValue(response({ unit: 'gost.service', active_state: 'custom' }))
    api.getGostLogs.mockRejectedValueOnce(apiError('日志不可用')).mockResolvedValue(response({ entries: [] }))
    const view = mountService('offline')
    expect(view.text()).toContain('节点当前未在线')
    expect(api.getGostService).not.toHaveBeenCalled()
    expect(api.getNodeFirewallStatus).not.toHaveBeenCalled()
    await click(view, '刷新')
    expect(api.getNodeFirewallStatus).toHaveBeenCalledExactlyOnceWith(node.id)
    expect(view.text()).toContain('无法读取 Gost 状态')
    await view.get('button[aria-controls="node-gost-logs"]').trigger('click')
    await flushPromises()
    expect(view.text()).toContain('日志不可用')
    await view.setProps({ node: { ...node, status: 'online' } })
    await flushPromises()
    expect(api.getNodeFirewallStatus).toHaveBeenCalledTimes(2)
    expect(view.get('.ops-status').text()).toBe('custom')
    expect(view.get('.ops-status').classes()).toContain('is-muted')
    expect(button(view, '重启 Gost').element.disabled).toBe(true)
  })

  it('shows firewall sync failures and the last successful sync while the Gost status read fails', async () => {
    const lastSync = '2026-11-13T03:30:00Z'
    const state = { supported: true, managed: true, last_sync: lastSync, backend: 'nftables' }
    api.getGostService.mockRejectedValue(apiError('无法读取 Gost 状态'))
    api.getNodeFirewallStatus.mockResolvedValueOnce(response({ ...state, synchronized: false, last_error: 'missing firewall jump' }))
      .mockResolvedValueOnce(response({ ...state, synchronized: true, last_error: null }))
    const view = mountService()
    await flushPromises()
    const health = view.get('#node-firewall-status')
    expect(statusFields(health)).toMatchObject({ 自动放行后端: 'nftables', 同步状态: '同步异常' })
    expect(health.get('.firewall-startup').text()).toBe('已启用')
    expect(health.text()).toContain('inet input 链')
    expect(health.text()).toContain('missing firewall jump')
    expect(health.text()).toContain('最近成功同步')
    expect(health.get('time').attributes('datetime')).toBe(lastSync)
    expect(view.text()).toContain('无法读取 Gost 状态')

    await click(view, '刷新')
    expect(statusFields(health).同步状态).toBe('已同步')
    expect(health.text()).not.toContain('missing firewall jump')
    expect(health.find('.firewall-enable').exists()).toBe(false)
  })

  it.each([
    [false, 'explicitly_disabled', '节点配置将 firewall.enabled 设为 false。'],
    [false, 'not_configured', '节点未配置自动放行，默认关闭。'],
    [true, 'invalid_configuration', '节点的防火墙配置无效。'],
    [true, 'unsupported_environment', '当前节点环境不支持自动放行。'],
    [false, undefined, '节点配置未启用自动放行。']
  ])('explains disabled automatic rules (%s, %s)', async (managed, disabledReason, message) => {
    api.getGostService.mockResolvedValue(response(active))
    api.getNodeFirewallStatus.mockResolvedValue(response({
      backend: 'nftables', supported: false, managed, synchronized: false, disabled_reason: disabledReason
    }))
    const view = mountService()
    await flushPromises()
    const health = view.get('#node-firewall-status')
    expect(statusFields(health)).toEqual({ 自动放行后端: 'nftables' })
    expect(health.get('.firewall-startup').text()).toBe('未启用')
    expect(health.text()).toContain(message)
    expect(health.find('.firewall-enable').exists()).toBe(disabledReason !== 'unsupported_environment')
    expect(api.getNodeNftConfig).not.toHaveBeenCalled()
  })

  it('enables UFW remotely and displays the separately refreshed synchronization status', async () => {
    const pending = deferred()
    api.getGostService.mockResolvedValue(response(active))
    api.getNodeFirewallStatus
      .mockResolvedValueOnce(response({ backend: 'ufw', supported: true, managed: false, disabled_reason: 'not_configured' }))
      .mockResolvedValueOnce(response({ backend: 'ufw', supported: true, managed: true, synchronized: true }))
    api.enableNodeFirewall.mockReturnValueOnce(pending.promise)
    const view = mountService()
    await flushPromises()
    const health = view.get('#node-firewall-status')
    expect(statusFields(health).自动放行后端).toBe('UFW')
    expect(health.get('.firewall-startup').text()).toBe('未启用')
    expect(health.text()).not.toContain('查看开启步骤')
    expect(health.find('details').exists()).toBe(false)
    await click(health, '启用自动放行')
    expect(api.enableNodeFirewall).toHaveBeenCalledExactlyOnceWith(node.id)
    expect(health.attributes('aria-busy')).toBe('true')
    expect(button(health, '启用自动放行').element.disabled).toBe(true)
    expect(button(view, '刷新').element.disabled).toBe(true)

    pending.resolve(response({ backend: 'ufw', supported: true, managed: true, synchronized: false }))
    await flushPromises()
    expect(statusFields(health).同步状态).toBe('待同步')
    expect(health.get('.firewall-startup').text()).toBe('已启用')
    expect(messages.success).toHaveBeenCalledWith('自动放行配置已保存')
    expect(health.find('.firewall-enable').exists()).toBe(false)

    await click(view, '刷新')
    expect(statusFields(health).同步状态).toBe('已同步')
  })

  it('allows remote enable after a previously invalid nftables configuration is repaired', async () => {
    api.getGostService.mockResolvedValue(response(active))
    api.getNodeFirewallStatus.mockResolvedValue(response({
      backend: 'nftables', supported: false, managed: true, disabled_reason: 'invalid_configuration', last_error: 'missing input_chain'
    }))
    api.enableNodeFirewall.mockResolvedValue(response({ backend: 'nftables', supported: true, managed: true, synchronized: true }))
    const view = mountService()
    await flushPromises()
    const health = view.get('#node-firewall-status')
    expect(health.get('[role="alert"]').text()).toContain('missing input_chain')
    await click(health, '启用自动放行')
    expect(api.enableNodeFirewall).toHaveBeenCalledExactlyOnceWith(node.id)
    expect(health.get('.firewall-startup').text()).toBe('已启用')
    expect(statusFields(health).同步状态).toBe('已同步')
    expect(health.find('[role="alert"]').exists()).toBe(false)
  })

  it('shows a delegated firewalld failure and allows a retry', async () => {
    api.getGostService.mockResolvedValue(response(active))
    api.getNodeFirewallStatus.mockResolvedValue(response({
      backend: 'firewalld', supported: true, managed: false, disabled_reason: 'explicitly_disabled'
    }))
    api.enableNodeFirewall.mockRejectedValueOnce(apiError('请先配置 firewalld 入站 policy'))
      .mockResolvedValueOnce(response({ backend: 'firewalld', supported: true, managed: true, synchronized: false, last_error: 'policy is not active' }))
    const view = mountService()
    await flushPromises()
    const health = view.get('#node-firewall-status')
    await click(health, '启用自动放行')
    expect(health.get('[role="alert"]').text()).toBe('请先配置 firewalld 入站 policy')
    expect(health.get('.firewall-startup').text()).toBe('未启用')
    expect(messages.success).not.toHaveBeenCalled()
    await click(health, '启用自动放行')
    expect(api.enableNodeFirewall).toHaveBeenCalledTimes(2)
    expect(health.get('.firewall-startup').text()).toBe('已启用')
    expect(statusFields(health).同步状态).toBe('同步异常')
    expect(health.get('[role="alert"]').text()).toBe('policy is not active')
  })

  it('explains when the daemon can report status but needs an upgrade to enable remotely', async () => {
    api.getGostService.mockResolvedValue(response(active))
    api.getNodeFirewallStatus.mockResolvedValue(response({ backend: 'ufw', supported: true, managed: false }))
    api.enableNodeFirewall.mockRejectedValueOnce({ response: { status: 501 } })
    const view = mountService()
    await flushPromises()
    const health = view.get('#node-firewall-status')
    await click(health, '启用自动放行')
    expect(health.get('[role="alert"]').text()).toBe('请升级节点的 yagp-daemon 后再启用自动放行。')
    expect(health.find('.firewall-enable').exists()).toBe(false)
    await click(view, '刷新')
    expect(health.find('[role="alert"]').exists()).toBe(false)
    expect(button(health, '启用自动放行').element.disabled).toBe(false)
  })

  it('does not offer remote enable for an unrecognized backend', async () => {
    api.getGostService.mockResolvedValue(response(active))
    api.getNodeFirewallStatus.mockResolvedValue(response({ supported: false, managed: false, disabled_reason: 'not_configured' }))
    const view = mountService()
    await flushPromises()
    const health = view.get('#node-firewall-status')
    expect(statusFields(health).自动放行后端).toBe('未识别')
    expect(health.find('.firewall-enable').exists()).toBe(false)
  })

  it('shows older daemon support independently of the running Gost service', async () => {
    api.getGostService.mockResolvedValue(response(active))
    api.getNodeFirewallStatus.mockRejectedValue({ response: { status: 501 } })
    const view = mountService()
    await flushPromises()
    expect(view.get('#node-firewall-status').text()).toContain('守护程序版本较旧')
    expect(view.text()).toContain('运行中')
  })

  it('shows the UFW requirement without nftables delegation guidance', async () => {
    api.getGostService.mockResolvedValue(response(active))
    api.getNodeFirewallStatus.mockResolvedValue(response({
      backend: 'ufw', supported: false, managed: true, synchronized: false, last_error: 'UFW is inactive'
    }))
    const view = mountService()
    await flushPromises()
    const health = view.get('#node-firewall-status')
    expect(statusFields(health)).toMatchObject({ 自动放行后端: 'UFW', 同步状态: '后端不可用' })
    expect(health.get('.firewall-startup').text()).toBe('已启用')
    expect(health.text()).toContain('UFW 已启用并运行')
    expect(health.text()).toContain('UFW is inactive')
    expect(health.text()).not.toContain('inet input 链')
  })

  it('shows the firewalld policy requirement and refreshes after delegation is ready', async () => {
    const state = { backend: 'firewalld', supported: true, managed: true }
    api.getGostService.mockResolvedValue(response(active))
    api.getNodeFirewallStatus
      .mockResolvedValueOnce(response({ ...state, synchronized: false }))
      .mockResolvedValueOnce(response({ ...state, synchronized: true, last_error: null }))
    const view = mountService()
    await flushPromises()
    const health = view.get('#node-firewall-status')
    expect(statusFields(health)).toMatchObject({ 自动放行后端: 'firewalld', 同步状态: '待同步' })
    expect(health.get('.firewall-startup').text()).toBe('已启用')
    expect(health.text()).toContain('管理员配置的入站 policy')
    expect(health.text()).not.toContain('inet input 链')

    await click(view, '刷新')
    expect(statusFields(health).同步状态).toBe('已同步')
  })
})

describe('nftables preview', () => {
  const activeRules = '{"nftables":[{"quota":{"table":"yagp_quota","bytes":9223372036854775807}}]}'

  it('retries a read failure and displays active rules without editing controls', async () => {
    api.getNodeNftConfig.mockRejectedValueOnce(apiError('节点离线'))
      .mockResolvedValueOnce(response({ config: activeRules, live: true }))
    wrapper = mountNodeComponent(NodeNftPreview, { node })
    expect(wrapper.text()).toContain('正在读取 nftables 规则')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('节点离线')
    await click(wrapper, '重新加载')
    expect(api.getNodeNftConfig).toHaveBeenCalledWith(node.id)
    expect(wrapper.get('pre').text()).toBe(activeRules)
    expect(wrapper.find('textarea').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('应用到节点')
    expect(api.getNodeFirewallStatus).not.toHaveBeenCalled()
  })

  it('labels saved rules from older nodes and shows when no live rules are configured', async () => {
    api.getNodeNftConfig.mockResolvedValueOnce(response({ config: activeRules, live: false }))
      .mockResolvedValueOnce(response({ config: '{"nftables":[]}', live: true }))
    wrapper = mountNodeComponent(NodeNftPreview, { node })
    await flushPromises()
    expect(wrapper.text()).toContain('旧版守护程序仅提供上次保存的规则')
    expect(wrapper.get('pre').text()).toBe(activeRules)
    await click(wrapper, '刷新')
    expect(wrapper.text()).toContain('此节点暂无 yagp 管理的 nftables 规则。')
    expect(wrapper.find('pre').exists()).toBe(false)
  })
})

describe('node upgrades', () => {
  const available = {
    available: true,
    daemon_version: '1.0', latest_daemon_version: '1.1', daemon_update_available: true,
    auto_upgrade_daemon: true
  }
  const manual = (view) => view.get('button[aria-label="手动升级 yagp-daemon"]')
  const openDaemonPanel = () => {
    wrapper = mountNodeComponent(NodeUpgradePanel, { node, component: 'daemon' })
    return wrapper
  }

  it('queries the latest Gost release only through a confirmed manual upgrade request', async () => {
    const installed = {
      ...available,
      gost_version: 'v3.2.7', latest_gost_version: 'v3.2.8', gost_update_available: false, auto_upgrade_gost: true
    }
    api.getNodeUpgradeStatus.mockResolvedValue(response(installed))
    const view = open(NodeUpgradeDialog)
    await flushPromises()
    expect(view.findAll('.upgrade-versions dd').map(version => version.text())).toEqual(['v3.2.7'])
    expect(view.find('[role="switch"]').exists()).toBe(false)
    expect(view.find('button[aria-label="手动升级 yagp-daemon"]').exists()).toBe(false)
    const gost = button(view, '升级 Gost 至最新稳定版')
    expect(gost.element.disabled).toBe(false)
    await click(view, '刷新版本状态')
    expect(api.upgradeNodeComponent).not.toHaveBeenCalled()

    messages.confirm.mockRejectedValueOnce('cancel')
    await gost.trigger('click')
    await flushPromises()
    expect(messages.confirm).toHaveBeenCalledWith(expect.stringContaining('最新稳定版'), '升级 Gost', expect.anything())
    expect(api.upgradeNodeComponent).not.toHaveBeenCalled()

    const upgrade = deferred()
    api.upgradeNodeComponent.mockRejectedValueOnce(apiError('没有可用的更高稳定版本')).mockReturnValueOnce(upgrade.promise)
    await gost.trigger('click')
    await flushPromises()
    expect(view.get('[role="alert"]').text()).toBe('没有可用的更高稳定版本')
    await gost.trigger('click')
    await flushPromises()
    expect(api.upgradeNodeComponent).toHaveBeenLastCalledWith(node.id, 'gost')
    expect(button(view, '关闭').element.disabled).toBe(true)
    upgrade.resolve(response({ component: 'gost', target_version: 'v3.2.8', state: 'queued' }))
    await flushPromises()
    expect(view.get('.upgrade-job').text()).toContain('Gost升级v3.2.8等待执行')
    expect(gost.element.disabled).toBe(true)

    api.getNodeUpgradeStatus.mockResolvedValueOnce(response({
      ...installed, gost_version: 'v3.2.8', component: 'gost', target_version: 'v3.2.8', state: 'succeeded'
    }))
    await click(view, '刷新版本状态')
    expect(view.get('[aria-labelledby="node-upgrade-gost"] dd').text()).toBe('v3.2.8')
    expect(view.get('.upgrade-job').text()).toContain('升级成功')
    await click(view, '关闭')
    expect(view.emitted('update:modelValue')).toEqual([[false]])
  })

  it('blocks a Gost upgrade while a daemon upgrade is active', async () => {
    const upgrading = { ...available, gost_version: 'v3.2.7', component: 'daemon', target_version: '1.1', state: 'running' }
    api.getNodeUpgradeStatus.mockResolvedValueOnce(response(upgrading))
      .mockResolvedValueOnce(response({ ...upgrading, state: 'succeeded' }))
    const view = open(NodeUpgradeDialog)
    await flushPromises()
    const gost = button(view, '升级 Gost 至最新稳定版')
    expect(gost.element.disabled).toBe(true)
    expect(view.get('.upgrade-job').text()).toContain('yagp-daemon升级1.1正在升级')
    await click(view, '刷新版本状态')
    expect(gost.element.disabled).toBe(false)
    expect(view.find('.upgrade-job').exists()).toBe(false)
  })

  it('keeps policy editable on an unavailable node and commits changes only on success', async () => {
    api.getNodeUpgradeStatus.mockRejectedValueOnce(apiError('节点维护中'))
      .mockResolvedValueOnce(response({ available: false, reason: '需要升级守护程序' }))
      .mockResolvedValueOnce(response({ ...available, daemon_version: '', latest_daemon_version: '', daemon_update_available: false }))
      .mockResolvedValueOnce(response({ ...available, latest_daemon_version: '1.0', daemon_update_available: false }))
    const view = openDaemonPanel()
    await nextTick()
    expect(view.text()).toContain('正在读取版本与升级状态')
    await flushPromises()
    expect(view.get('[role="alert"]').text()).toBe('节点维护中')
    await click(view, '刷新版本状态')
    expect(view.text()).toContain('需要升级守护程序')
    expect(view.text()).toContain('自动升级设置仍可编辑')
    expect(manual(view).element.disabled).toBe(true)

    const daemon = view.getComponent(ElSwitch)
    api.updateNodeUpgradePolicy.mockRejectedValueOnce(new Error('权限不足')).mockResolvedValue(response({}))
    daemon.vm.$emit('change', true)
    await flushPromises()
    expect(view.get('[role="alert"]').text()).toBe('权限不足')
    expect(daemon.props('modelValue')).toBe(false)
    daemon.vm.$emit('change', true)
    await flushPromises()
    expect(api.updateNodeUpgradePolicy).toHaveBeenLastCalledWith(node.id, { auto_upgrade_daemon: true })
    expect(daemon.props('modelValue')).toBe(true)
    expect(messages.success).toHaveBeenCalledWith('已保存自动升级设置')
    await click(view, '刷新版本状态')
    expect(view.text()).toContain('版本待确认')
    await click(view, '刷新版本状态')
    expect(view.text()).toContain('暂无更新')
  })

  it('preserves an upgrade job during restarts, pauses polling and can resume it manually', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] })
    api.getNodeUpgradeStatus.mockResolvedValue(response(available))
    const view = openDaemonPanel()
    await flushPromises()
    expect(view.text()).toContain('有可用更新')
    expect(view.findAll('.upgrade-versions dd').map(version => version.text())).toEqual(['1.0', '1.1'])
    expect(view.find('[aria-labelledby="node-upgrade-gost"]').exists()).toBe(false)
    messages.confirm.mockRejectedValueOnce('cancel')
    await manual(view).trigger('click')
    await flushPromises()
    expect(messages.confirm).toHaveBeenCalledWith(expect.stringContaining('当前面板版本 1.1'), '升级 yagp-daemon', expect.anything())
    expect(api.upgradeNodeComponent).not.toHaveBeenCalled()

    api.upgradeNodeComponent.mockRejectedValueOnce({ isAxiosError: true }).mockResolvedValueOnce(response({
      component: 'daemon', target_version: '1.1', state: 'queued'
    }))
    await manual(view).trigger('click')
    await flushPromises()
    expect(view.get('[role="alert"]').text()).toContain('无法确认升级请求已提交')
    await manual(view).trigger('click')
    await flushPromises()
    expect(messages.confirm).toHaveBeenLastCalledWith(expect.stringContaining('当前面板版本 1.1'), '升级 yagp-daemon', expect.anything())
    expect(api.upgradeNodeComponent).toHaveBeenLastCalledWith(node.id, 'daemon')
    expect(view.text()).toContain('等待执行')
    expect(messages.success).toHaveBeenCalledWith('升级请求已提交')

    api.getNodeUpgradeStatus.mockRejectedValueOnce(new Error('restarting'))
    await vi.advanceTimersByTimeAsync(4000)
    expect(view.text()).toContain('系统会继续查询')
    api.getNodeUpgradeStatus.mockResolvedValueOnce(response({ available: false }))
    await vi.advanceTimersByTimeAsync(4000)
    expect(view.text()).toContain('等待执行')
    expect(view.text()).toContain('节点当前无法执行手动升级')
    api.getNodeUpgradeStatus.mockResolvedValue(response({ ...available, component: 'daemon', target_version: '1.1', state: 'running' }))
    await vi.advanceTimersByTimeAsync(292000)
    expect(view.text()).toContain('已暂停自动查询')
    const callsAtDeadline = api.getNodeUpgradeStatus.mock.calls.length
    await vi.advanceTimersByTimeAsync(4000)
    expect(api.getNodeUpgradeStatus).toHaveBeenCalledTimes(callsAtDeadline)

    api.getNodeUpgradeStatus.mockResolvedValueOnce(response({ ...available, component: 'daemon', target_version: '1.1', state: 'failed', error: '磁盘已满' }))
    await click(view, '刷新版本状态')
    expect(view.get('.upgrade-job').attributes('role')).toBe('alert')
    expect(view.text()).toContain('磁盘已满')
    expect(view.text()).not.toContain('已暂停自动查询')
  })

  it('does not schedule another refresh after unmounting a pending poll', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    api.getNodeUpgradeStatus.mockResolvedValueOnce(response({ ...available, component: 'daemon', state: 'queued' }))
    const view = openDaemonPanel()
    await flushPromises()
    const request = deferred()
    api.getNodeUpgradeStatus.mockReturnValueOnce(request.promise)
    await vi.advanceTimersByTimeAsync(4000)
    expect(api.getNodeUpgradeStatus).toHaveBeenCalledTimes(2)
    view.unmount()
    wrapper = undefined
    request.resolve(response({ ...available, component: 'daemon', state: 'running' }))
    await nextTick()
    await vi.advanceTimersByTimeAsync(8000)
    expect(api.getNodeUpgradeStatus).toHaveBeenCalledTimes(2)
  })
})
