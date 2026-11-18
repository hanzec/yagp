export const needsStop = rule => rule.status === 'running' || !!rule.service_id
export const hasTunnelBindings = rule => !!(rule.linked_tunnels?.length || rule.linked_tunnel_id)
