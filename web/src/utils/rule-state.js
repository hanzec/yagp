export const needsStop = rule => rule.status === 'running' || !!rule.service_id
export const hasTunnelBindings = rule => !!(rule.linked_tunnels?.length || rule.linked_tunnel_id)

export function appliedRuleConfig(rule) {
  const applied = rule?.applied_endpoint
  if (!applied) return rule
  return {
    ...rule,
    protocol: applied.Protocol,
    listen_ip: applied.ListenIP,
    listen_port: applied.ListenPort,
    protocol_options: applied.ProtocolOptions,
    targets: applied.Targets,
    use_clash_ruleset: applied.UseClashRuleset,
    use_original_target: applied.UseOriginalTarget,
    ruleset_ids: applied.RulesetIDs,
    manual_domains: applied.ManualDomains,
    strategy: applied.Strategy,
    enable_tls: applied.EnableTLS
  }
}

export const tunnelExitConfig = tunnel => tunnel.status === 'running' ? appliedRuleConfig(tunnel.exit_rule) : tunnel.exit_rule
