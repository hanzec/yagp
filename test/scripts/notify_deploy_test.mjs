import assert from 'node:assert/strict'
import { test } from 'node:test'
import { waitForUpdate } from '../../.github/scripts/notify-deploy.mjs'

const origin = 'https://receiver.example'
const job = { status_url: '/api/deploy/status/accepted-job', status_websocket: true }
const running = (logs, log_offset) => ({ state: 'running', mode: 'compose', logs, log_offset })
const succeeded = (logs, log_offset) => ({ ...running(logs, log_offset), state: 'succeeded', exit_code: 0 })

function mockSocket(t, updates, finalEvent) {
  const urls = []
  const original = globalThis.WebSocket
  t.after(() => { globalThis.WebSocket = original })
  globalThis.WebSocket = class extends EventTarget {
    constructor(url) {
      super()
      urls.push(url)
      if (urls.length > 1) throw new Error('unexpected websocket retry')
      queueMicrotask(() => {
        for (const update of updates) this.dispatchEvent(new MessageEvent('message', { data: JSON.stringify(update) }))
        if (finalEvent) this.dispatchEvent(new Event(finalEvent))
      })
    }
    close() {}
  }
  return urls
}

test('failed handshake reads the accepted job over HTTPS', async t => {
  const sockets = mockSocket(t, [], 'error')
  const fetch = t.mock.method(globalThis, 'fetch', async () => Response.json(succeeded('finished', 0)))
  await waitForUpdate(origin, job, 'compose')
  assert.deepEqual(sockets, ['wss://receiver.example' + job.status_url])
  assert.equal(fetch.mock.callCount(), 1)
  const [url, options] = fetch.mock.calls[0].arguments
  assert.equal(url, origin + job.status_url)
  assert.equal(options.method ?? 'GET', 'GET')
})

test('HTTPS resumes a shifted 64 KiB log window at its byte offset', async t => {
  const record = '节点' + 'x'.repeat(1017) + '\n'
  assert.equal(Buffer.byteLength(record), 1024)
  const window = record.repeat(64)
  const initialOffset = 4096
  mockSocket(t, [running(window, initialOffset)], 'close')
  t.mock.method(globalThis, 'fetch', async () => Response.json(succeeded(window, initialOffset + 1024)))
  const output = t.mock.method(console, 'log', () => {})
  await waitForUpdate(origin, job, 'compose')
  const printed = output.mock.calls.filter(call => call.arguments[0].startsWith('server | '))
  assert.equal(printed.length, 65)
  assert.ok(printed.every(call => call.arguments[0] === 'server | ' + record.trimEnd()))
})

test('older receivers stream logs without offsets', async t => {
  mockSocket(t, [running('first\nsecond\n'), succeeded('second\nthird\n')])
  const output = t.mock.method(console, 'log', () => {})
  await waitForUpdate(origin, job, 'compose')
  const lines = output.mock.calls.map(call => call.arguments[0]).filter(line => line.startsWith('server | '))
  assert.deepEqual(lines, ['server | first', 'server | second', 'server | third'])
})

test('a failed job received over HTTPS still fails the workflow', async t => {
  mockSocket(t, [], 'close')
  t.mock.method(globalThis, 'fetch', async () => Response.json({ mode: 'compose', state: 'failed', exit_code: 7 }))
  await assert.rejects(waitForUpdate(origin, job, 'compose'), /exit code 7/)
})
