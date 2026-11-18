export function layoutTunnelTopology(tunnels, compact = false) {
  const nodeWidth = compact ? 64 : 176
  const nodeHeight = 88
  const columnGap = compact ? 40 : 176
  const rowGap = compact ? 32 : 56
  const padding = compact ? 8 : 16
  const headerHeight = 40
  const labelWidth = compact ? 32 : 152
  const labelHeight = compact ? 24 : 28
  const nodes = new Map()
  const endpoint = (tunnel, side) => {
    const reported = tunnel[`${side}_node`] || tunnel[`${side}_rule`]?.node
    const id = String(tunnel[`${side}_node_id`] ?? reported?.id ?? `${side}-${tunnel.id}`)
    if (!nodes.has(id)) nodes.set(id, { id, name: reported?.name, incoming: 0, outgoing: 0, next: [] })
    const node = nodes.get(id)
    if (!node.name && reported?.name) node.name = reported.name
    return node
  }
  const links = tunnels.map(tunnel => {
    const source = endpoint(tunnel, 'entry'), target = endpoint(tunnel, 'exit')
    source.outgoing++
    target.incoming++
    source.next.push(target)
    return { id: tunnel.id, name: tunnel.name || `链路 #${tunnel.id}`, source, target }
  })
  assignColumns(nodes)
  const columns = []
  for (const node of nodes.values()) (columns[node.column] ||= []).push(node)
  const contentHeight = columns.reduce((largest, column) => Math.max(largest, column.length), 1) * (nodeHeight + rowGap) - rowGap
  for (const [column, members] of columns.entries()) {
    members.sort((left, right) => left.id.localeCompare(right.id, undefined, { numeric: true }))
    members.forEach((node, row) => {
      node.name ||= `节点 #${node.id}`
      node.x = column * (nodeWidth + columnGap)
      node.y = (contentHeight - (members.length * (nodeHeight + rowGap) - rowGap)) / 2 + row * (nodeHeight + rowGap)
      node.width = nodeWidth
      node.height = nodeHeight
    })
  }
  const preceding = new Map()
  for (const { source, target } of links) {
    if (target.column !== source.column + 1) continue
    if (!preceding.has(target)) preceding.set(target, new Set())
    preceding.get(target).add(source)
  }
  for (const members of columns) {
    if (members.length !== 1) continue
    const parents = preceding.get(members[0])
    if (parents?.size === 1) members[0].y = parents.values().next().value.y
  }
  const pairs = new Map()
  const directedCounts = new Map()
  for (const link of links) {
    const key = [link.source.id, link.target.id].sort().join('\0')
    if (!pairs.has(key)) pairs.set(key, [])
    pairs.get(key).push(link)
    link.directionKey = `${link.source.id}\0${link.target.id}`
    link.parallelIndex = (directedCounts.get(link.directionKey) || 0) + 1
    directedCounts.set(link.directionKey, link.parallelIndex)
  }
  let outsideLane = 0
  for (const pair of pairs.values()) pair.forEach((link, index) => {
    const { source, target } = link
    link.parallelCount = directedCounts.get(link.directionKey)
    delete link.directionKey
    let sx, sy, tx, ty, c1x, c1y, c2x, c2y
    if (source === target) {
      sx = source.x + nodeWidth * .25
      tx = source.x + nodeWidth * .75
      sy = ty = source.y
      c1x = sx - 20
      c2x = tx + 20
      c1y = c2y = -48 - outsideLane++ * 40
    } else if (target.column - source.column > 1) {
      sx = source.x + nodeWidth / 2
      tx = target.x + nodeWidth / 2
      sy = source.y
      ty = target.y
      c1x = sx
      c2x = tx
      c1y = c2y = -40 - outsideLane++ * 40
    } else if (source.column === target.column) {
      const side = source.y < target.y ? 1 : -1
      sx = source.x + (side > 0 ? nodeWidth : 0)
      tx = target.x + (side > 0 ? nodeWidth : 0)
      sy = source.y + nodeHeight / 2
      ty = target.y + nodeHeight / 2
      c1x = sx + side * (compact ? 32 + index * 12 : 80 + index * 30)
      c2x = tx + side * (compact ? 32 + index * 12 : 80 + index * 30)
      c1y = sy
      c2y = ty
    } else {
      sx = source.x + nodeWidth
      tx = target.x
      sy = source.y + nodeHeight / 2
      ty = target.y + nodeHeight / 2
      const offset = (index - (pair.length - 1) / 2) * (compact ? 40 : 48)
      c1x = sx + columnGap * .45
      c2x = tx - columnGap * .45
      c1y = sy + offset
      c2y = ty + offset
    }
    link.points = [{ x: sx, y: sy }, { x: c1x, y: c1y }, { x: c2x, y: c2y }, { x: tx, y: ty }]
    link.anchorX = link.labelX = (sx + 3 * c1x + 3 * c2x + tx) / 8
    link.anchorY = link.labelY = (sy + 3 * c1y + 3 * c2y + ty) / 8
    link.description = `${link.name}：${source.name} → ${target.name}`
    if (link.parallelCount > 1) link.description += `，相同入口与出口的第 ${link.parallelIndex}/${link.parallelCount} 条链路`
  })
  // Crossings can share a midpoint. Move labels rather than changing the graph or keyboard order.
  const positionedNodes = [...nodes.values()]
  const labels = []
  for (const link of links) {
    for (let slot = 0; ; slot++) {
      const y = link.anchorY + (slot ? Math.ceil(slot / 2) * (labelHeight + 8) * (slot % 2 ? -1 : 1) : 0)
      const overlapsLabel = labels.some(label => Math.abs(label.x - link.anchorX) < labelWidth + 8 && Math.abs(label.y - y) < labelHeight + 6)
      const overlapsNode = positionedNodes.some(node => link.anchorX + labelWidth / 2 > node.x - 3 && link.anchorX - labelWidth / 2 < node.x + nodeWidth + 3 && y + labelHeight / 2 > node.y - 3 && y - labelHeight / 2 < node.y + nodeHeight + 3)
      if (overlapsLabel || overlapsNode) continue
      link.labelY = y
      labels.push({ x: link.labelX, y })
      break
    }
  }
  let minX = 0, minY = 0, maxX = nodeWidth, maxY = contentHeight
  const include = point => {
    minX = Math.min(minX, point.x)
    minY = Math.min(minY, point.y)
    maxX = Math.max(maxX, point.x)
    maxY = Math.max(maxY, point.y)
  }
  for (const node of nodes.values()) {
    include({ x: node.x, y: node.y })
    include({ x: node.x + nodeWidth, y: node.y + nodeHeight })
  }
  for (const link of links) {
    // A Bézier curve stays inside the convex hull of its four control points.
    for (const point of link.points) include(point)
    include({ x: link.labelX - labelWidth / 2, y: link.labelY - labelHeight / 2 })
    include({ x: link.labelX + labelWidth / 2, y: link.labelY + labelHeight / 2 })
  }
  const offsetX = padding - minX, offsetY = headerHeight + padding - minY
  for (const node of nodes.values()) {
    node.x += offsetX
    node.y += offsetY
    delete node.next
  }
  for (const link of links) {
    const p = link.points.map(point => ({ x: point.x + offsetX, y: point.y + offsetY }))
    link.path = `M ${p[0].x} ${p[0].y} C ${p[1].x} ${p[1].y}, ${p[2].x} ${p[2].y}, ${p[3].x} ${p[3].y}`
    link.anchorX += offsetX
    link.labelX += offsetX
    link.anchorY += offsetY
    link.labelY += offsetY
    delete link.points
  }
  return {
    nodes: positionedNodes, links, labelWidth, labelHeight,
    columns: columns.map((members, index) => ({ index, ...columnTitle(members), centerX: offsetX + index * (nodeWidth + columnGap) + nodeWidth / 2 })),
    width: maxX - minX + padding * 2, height: maxY - minY + headerHeight + padding * 2
  }
}

function assignColumns(nodes) {
  const states = new Map(), stack = [], components = []
  let sequence = 0
  const visit = node => {
    const state = { order: sequence, low: sequence++, stacked: true }
    states.set(node, state)
    stack.push(node)
    for (const next of node.next) {
      if (!states.has(next)) {
        visit(next)
        state.low = Math.min(state.low, states.get(next).low)
      } else if (states.get(next).stacked) state.low = Math.min(state.low, states.get(next).order)
    }
    if (state.low !== state.order) return
    const component = { members: [], next: new Set(), incoming: 0, column: 0 }
    let member
    do {
      member = stack.pop()
      states.get(member).stacked = false
      member.component = component
      component.members.push(member)
    } while (member !== node)
    components.push(component)
    const cyclic = component.members.length > 1 || node.next.includes(node)
    for (const member of component.members) member.cyclic = cyclic
  }
  for (const node of nodes.values()) if (!states.has(node)) visit(node)
  for (const node of nodes.values()) for (const next of node.next) {
    if (next.component === node.component || node.component.next.has(next.component)) continue
    node.component.next.add(next.component)
    next.component.incoming++
  }
  const ready = components.filter(component => !component.incoming)
  for (let index = 0; index < ready.length; index++) {
    const component = ready[index]
    for (const next of component.next) {
      next.column = Math.max(next.column, component.column + 1)
      if (--next.incoming === 0) ready.push(next)
    }
  }
  // Bring a short source branch next to its nearest successor without moving terminal layers.
  for (let index = ready.length - 1; index >= 0; index--) {
    const component = ready[index]
    if (!component.next.size) continue
    let nearest = Infinity
    for (const next of component.next) nearest = Math.min(nearest, next.column)
    component.column = nearest - 1
  }
  // Cycle members remain one physical node each, together in the same layer.
  for (const node of nodes.values()) {
    node.column = node.component.column
    delete node.component
  }
}

function columnTitle(nodes) {
  const roles = new Set(nodes.map(node => node.cyclic ? 'cycle' : !node.incoming ? 'entry' : !node.outgoing ? 'exit' : 'relay'))
  const labels = { entry: '入口', relay: '中继', cycle: '环路', exit: '出口' }
  const titles = { entry: '始发入口', relay: '中继节点', cycle: '循环连接', exit: '终点出口' }
  const ordered = ['entry', 'relay', 'cycle', 'exit'].filter(role => roles.has(role))
  return {
    title: ordered.length === 1 ? titles[ordered[0]] : ordered.map(role => labels[role]).join(' / '),
    shortTitle: ordered.length > 2 ? '混合节点' : ordered.map(role => labels[role]).join('·')
  }
}
