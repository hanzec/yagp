package service

import (
	"context"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"yagp/internal/model"
	"yagp/internal/repository"
	"yagp/internal/utils"
	"yagp/pkg/gost"
	"yagp/pkg/logger"
	"yagp/pkg/nodegrpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"
)

// RuleSyncService 规则状态同步服务
// 定时从 Gost 节点同步规则的真实运行状态
type RuleSyncService struct {
	nodeRepo            *repository.NodeRepository
	ruleRepo            *repository.RuleRepository
	tunnelRepo          *repository.TunnelRepository
	allowlist           *GostAllowlistService
	syncing             sync.Map
	unsavedTunnelConfig sync.Map
	ticker              *time.Ticker
	stopChan            chan struct{}
	wg                  sync.WaitGroup
}

// NewRuleSyncService 创建规则状态同步服务
func NewRuleSyncService(db *gorm.DB) *RuleSyncService {
	return &RuleSyncService{
		nodeRepo:   repository.NewNodeRepository(db),
		ruleRepo:   repository.NewRuleRepository(db),
		tunnelRepo: repository.NewTunnelRepository(db),
		allowlist:  NewGostAllowlistService(db),
		stopChan:   make(chan struct{}),
	}
}

// Start 启动定时同步任务（每 5 秒）
func (s *RuleSyncService) Start() {
	s.ticker = time.NewTicker(5 * time.Second)
	s.wg.Add(1)

	go func() {
		defer s.wg.Done()
		logger.Info("规则状态同步服务已启动 (5s 间隔)")

		// 立即执行一次
		s.syncAll()

		for {
			select {
			case <-s.ticker.C:
				s.syncAll()
			case <-s.stopChan:
				logger.Info("规则状态同步服务已停止")
				return
			}
		}
	}()
}

// Stop 停止同步服务
func (s *RuleSyncService) Stop() {
	if s.ticker != nil {
		s.ticker.Stop()
	}
	close(s.stopChan)
	s.wg.Wait()
}

// syncAll 同步所有节点的规则状态
func (s *RuleSyncService) syncAll() {
	var nodes []model.GostNode
	if err := s.nodeRepo.DB.Find(&nodes).Error; err != nil {
		logger.Errorf("[Sync] 获取节点列表失败: %v", err)
		return
	}

	nodesByID := make(map[uint]*model.GostNode, len(nodes))
	for i := range nodes {
		nodesByID[nodes[i].ID] = &nodes[i]
	}
	for _, node := range nodes {
		// 并发同步每个节点
		s.wg.Go(func() { s.syncNodeRules(node, nodesByID) })
	}
}

// syncNodeRules 同步单个节点的规则
func (s *RuleSyncService) syncNodeRules(node model.GostNode, nodesByID map[uint]*model.GostNode) {
	// A slow Gost update can outlast a sync interval. Finish it before reconciling this node again.
	if _, busy := s.syncing.LoadOrStore(node.ID, struct{}{}); busy {
		return
	}
	defer s.syncing.Delete(node.ID)

	client := utils.GetGostClient(&node)
	configErr := s.reconcileNode(client, node, nodesByID)
	if configErr != nil {
		logger.Debugf("[Sync] 获取节点 %d (%s) 配置失败: %v", node.ID, node.Name, configErr)
	}
	s.updateNodeHealth(node, configErr)
}

func (s *RuleSyncService) reconcileNode(client *gost.Client, node model.GostNode, nodesByID map[uint]*model.GostNode) error {
	endpointTunnelRuntime.RLock()
	defer endpointTunnelRuntime.RUnlock()

	gostCfg, err := s.getNodeConfig(client, node)
	if err != nil {
		return err
	}

	// 提取节点上的 Service 状态
	serviceStates := make(map[string]string)
	for _, svc := range gostCfg.Services {
		state := "stopped"
		if svc.Status != nil {
			state = svc.Status.State
		}
		serviceStates[svc.Name] = state
	}

	// 1. 同步规则状态
	rules, err := s.ruleRepo.FindByEntryNodeID(node.ID)
	if err != nil {
		logger.Errorf("[Sync] 获取节点 %d 规则失败: %v", node.ID, err)
	} else {
		for _, r := range rules {
			s.syncRuleStatus(r, serviceStates)
		}
	}

	tunnels, err := s.tunnelRepo.FindByNodeID(node.ID)
	if err != nil {
		logger.Errorf("[Sync] 获取节点 %d 隧道规则失败: %v", node.ID, err)
		return nil
	}
	s.recordRelayTraffic(node.ID, tunnels, gostCfg.Services)
	changed, err := s.allowlist.syncRelays(client, gostCfg, node.ID, tunnels, nodesByID)
	if err != nil {
		logger.Errorf("[Sync] 下发隧道 IP 白名单到节点 %d 失败: %v", node.ID, err)
	}
	if changed {
		s.unsavedTunnelConfig.Store(node.ID, struct{}{})
	}
	s.syncTunnels(client, node.ID, tunnels, gostCfg.Chains, nodesByID)
	return nil
}

func (s *RuleSyncService) getNodeConfig(client *gost.Client, node model.GostNode) (*gost.GostConfig, error) {
	if node.TrafficLimitBytes == 0 && node.TrafficQuotaState == "" {
		return client.GetConfig()
	}
	unlock := lockTrafficQuota(node.ID)
	defer unlock()
	if node.TrafficLimitBytes == 0 {
		// Unblock even if the Gost admin API is temporarily down.
		if err := syncTrafficQuota(s.nodeRepo, client, node.ID, nil, time.Now()); err != nil {
			logger.Debugf("[Sync] 清除节点 %d 的流量配额失败: %v", node.ID, err)
		}
		return client.GetConfig()
	}
	current, err := client.GetConfig()
	if err != nil {
		return nil, err
	}
	if err := syncTrafficQuota(s.nodeRepo, client, node.ID, current.Services, time.Now()); err != nil {
		logger.Debugf("[Sync] 同步节点 %d 的流量配额失败: %v", node.ID, err)
	}
	return current, nil
}

func (s *RuleSyncService) syncTunnels(client *gost.Client, nodeID uint, tunnels []model.GostTunnel, chains []gost.ChainConfig, nodesByID map[uint]*model.GostNode) {
	chainsByName := make(map[string]gost.ChainConfig, len(chains))
	for _, chain := range chains {
		chainsByName[chain.Name] = chain
	}
	changed := false
	relayHostsByEntry := make(map[uint][]string)
	for _, t := range tunnels {
		if t.EntryNodeID == nodeID && t.EntryRuleID != nil && t.Status == model.TunnelStatusRunning {
			if host := relayHost(t.ExitRule.WithAppliedEndpoint(), nodesByID[t.ExitNodeID]); host != "" {
				relayHostsByEntry[*t.EntryRuleID] = append(relayHostsByEntry[*t.EntryRuleID], host)
			}
		}
	}
	for _, t := range tunnels {
		if t.EntryNodeID != nodeID {
			continue
		}
		// A legacy chain may remain after a failed start. Leave those resources for the explicit retry.
		if t.Status == model.TunnelStatusError || (t.EntryRuleID != nil && t.Status != model.TunnelStatusRunning) {
			continue
		}
		managedName := tunnelChainName(t.ID)
		chainName := t.ChainID
		if chainName == "" {
			chainName = managedName
		}
		_, exists := chainsByName[chainName]
		if t.EntryRuleID == nil {
			s.syncTunnelStatus(t, exists)
		}
		if !exists || chainName != managedName {
			continue
		}
		exitHost := relayHost(t.ExitRule.WithAppliedEndpoint(), nodesByID[t.ExitNodeID])
		clashChain := clashTunnelChainName(t.ID)
		for _, name := range []string{chainName, clashChain, sharedTransparentChainName(t.ID)} {
			addr, ok := chainsByName[name].TunnelRelayAddress()
			oldHost, _, err := net.SplitHostPort(addr)
			if !ok || err != nil || exitHost == "" || oldHost == exitHost {
				continue
			}
			if t.EntryRuleID != nil && name != chainName {
				if err := s.syncTransparentRelayHost(client, *t.EntryRuleID, exitHost, relayHostsByEntry[*t.EntryRuleID]); err != nil {
					logger.Warnf("[Sync] 更新隧道 %d 入口透明分流的出口地址失败: %v", t.ID, err)
					continue
				}
			}
			updated, err := client.UpdateTunnelRelayHost(name, exitHost)
			if err != nil {
				logger.Warnf("[Sync] 更新隧道 %d 转发链 %s 出口地址失败: %v", t.ID, name, err)
				continue
			}
			if updated {
				changed = true
				logger.Infof("[Sync] 隧道 %d 转发链 %s 出口地址变更: %s -> %s", t.ID, name, oldHost, exitHost)
			}
		}
	}
	if _, unsaved := s.unsavedTunnelConfig.Load(nodeID); changed || unsaved {
		if err := client.SaveConfig(); err != nil {
			s.unsavedTunnelConfig.Store(nodeID, struct{}{})
			logger.Warnf("[Sync] 保存节点 %d 隧道配置失败: %v", nodeID, err)
		} else {
			s.unsavedTunnelConfig.Delete(nodeID)
		}
	}
}

func (s *RuleSyncService) syncTransparentRelayHost(client *gost.Client, ruleID uint, exitHost string, hosts []string) error {
	rule, err := s.ruleRepo.FindByID(ruleID)
	if err != nil {
		return err
	}
	request := transparentRedirectRequest(rule.WithAppliedEndpoint(), exitHost)
	if len(hosts) > 1 {
		request.RelayHosts = hosts
	}
	request.RequireActive = true
	ctx := context.Background()
	prepared, err := client.ConfigureTransparentRedirect(ctx, request)
	if code := status.Code(err); code == codes.Unimplemented || code == codes.NotFound {
		return nil
	}
	if err != nil || !prepared.GetActive() {
		return err
	}
	request.Action = nodegrpc.TransparentRedirectAction_TRANSPARENT_REDIRECT_ACTION_APPLY
	request.Mark, request.DockerAddress = prepared.GetMark(), prepared.GetDockerAddress()
	_, err = client.ConfigureTransparentRedirect(ctx, request)
	return err
}

// syncRuleStatus 同步规则状态
func (s *RuleSyncService) syncRuleStatus(r model.GostRule, serviceStates map[string]string) {
	serviceID := r.ServiceID
	if serviceID == "" {
		serviceID = entranceServiceName(r.ID)
	}

	state, exists := serviceStates[serviceID]
	newStatus := utils.GostStateToRuleStatus(state)

	if exists && r.ServiceID == "" {
		if err := s.ruleRepo.UpdateRuntime(r.ID, serviceID, newStatus); err != nil {
			logger.Errorf("[Sync] 恢复规则 %d 服务标识失败: %v", r.ID, err)
		}
		return
	}
	if r.Status != newStatus {
		logger.Infof("[Sync] 规则 %d (%s) 状态变更: %s -> %s (Gost State: %s)", r.ID, r.Name, r.Status, newStatus, state)
		_ = s.ruleRepo.UpdateStatus(r.ID, newStatus)
	}
}

// syncTunnelStatus 同步隧道状态
func (s *RuleSyncService) syncTunnelStatus(t model.GostTunnel, exists bool) {
	var newStatus model.TunnelStatus
	if exists {
		newStatus = model.TunnelStatusRunning
	} else {
		newStatus = model.TunnelStatusStopped
	}

	if t.Status != newStatus {
		logger.Infof("[Sync] 隧道 %d (%s) 状态变更: %s -> %s (Chain Exists: %v)", t.ID, t.Name, t.Status, newStatus, exists)
		_ = s.tunnelRepo.UpdateStatus(t.ID, newStatus)
	}
}

func (s *RuleSyncService) recordRelayTraffic(nodeID uint, tunnels []model.GostTunnel, services []gost.ServiceConfig) {
	exits := make(map[string]model.GostTunnel)
	for _, tunnel := range tunnels {
		if tunnel.ExitNodeID != nodeID {
			continue
		}
		name := relayServiceName(tunnel.ID)
		if tunnel.ExitRule != nil && tunnel.ExitRule.Role == model.EndpointRoleExit {
			// Stopped links can retain an old runtime ID. Use the listener owned by the exit endpoint.
			if tunnel.ExitRule.ServiceID == "" || tunnel.ServiceID != tunnel.ExitRule.ServiceID {
				continue
			}
			name = tunnel.ServiceID
		}
		exits[name] = tunnel
	}
	for _, svc := range services {
		tunnel, exists := exits[svc.Name]
		if !exists || svc.Handler == nil || svc.Handler.Type != "relay" || svc.Status == nil || svc.Status.Stats == nil {
			continue
		}
		if err := s.recordRelaySample(tunnel, svc); err != nil {
			logger.Warnf("[Sync] 保存隧道 %d 的出口流量失败: %v", tunnel.ID, err)
		}
	}
}

func (s *RuleSyncService) recordRelaySample(tunnel model.GostTunnel, svc gost.ServiceConfig) error {
	observed := svc.Status.Stats
	created := svc.Status.CreateTime
	if created <= 0 || observed.InputBytes > math.MaxInt64 || observed.OutputBytes > math.MaxInt64 {
		return nil
	}

	previous := tunnel.RelayTraffic
	exit := tunnel.ExitRule
	if exit != nil && exit.Role == model.EndpointRoleExit {
		var err error
		previous, err = s.exitRelayCheckpoint(tunnel, svc.Name)
		if err != nil {
			return err
		}
	}
	lastInput, lastOutput := int64(observed.InputBytes), int64(observed.OutputBytes)
	input, output := lastInput, lastOutput
	if created == previous.ServiceCreatedAt {
		if lastInput == previous.LastInputBytes && lastOutput == previous.LastOutputBytes &&
			(exit == nil || exit.Role != model.EndpointRoleExit || exit.RelayCounterID == svc.Name) {
			return nil
		}
		input = counterIncrease(lastInput, previous.LastInputBytes)
		output = counterIncrease(lastOutput, previous.LastOutputBytes)
	}
	if exit != nil && exit.Role == model.EndpointRoleExit {
		return s.ruleRepo.RecordExitRelayTraffic(exit.ID, svc.Name, input, output, lastInput, lastOutput, created)
	}
	return s.tunnelRepo.RecordRelayTraffic(tunnel.ID, input, output, lastInput, lastOutput, created)
}

func (s *RuleSyncService) exitRelayCheckpoint(tunnel model.GostTunnel, serviceName string) (model.RelayTraffic, error) {
	exit := tunnel.ExitRule
	if exit.RelayCounterID == serviceName {
		return exit.RelayTraffic, nil
	}
	// A running listener can predate exit-level accounting. Its name identifies the old checkpoint owner.
	id, err := strconv.ParseUint(strings.TrimPrefix(serviceName, "relay-tunnel-"), 10, 64)
	if err != nil || relayServiceName(uint(id)) != serviceName {
		return model.RelayTraffic{}, fmt.Errorf("无效的 Relay 服务名 %q", serviceName)
	}
	if uint(id) == tunnel.ID {
		return tunnel.RelayTraffic, nil
	}
	return s.tunnelRepo.HistoricalRelayTraffic(uint(id), exit.ID, tunnel.ExitNodeID)
}
