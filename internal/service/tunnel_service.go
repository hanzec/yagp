// Package service 提供业务逻辑层服务
package service

import (
	"context"
	"crypto/rand"
	stderrors "errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"yagp/internal/dto"
	"yagp/internal/errors"
	"yagp/internal/model"
	"yagp/internal/repository"
	"yagp/internal/utils"
	"yagp/pkg/gost"
	"yagp/pkg/logger"

	"gorm.io/gorm"
)

// TunnelService 隧道服务
// 负责隧道的 CRUD 操作及启停控制
// 启动隧道时：在出口节点创建 Relay 服务，在入口节点创建 Chain 连接到出口节点
type TunnelService struct {
	tunnelRepo *repository.TunnelRepository
	nodeRepo   *repository.NodeRepository
	logService *LogService
	allowlist  *GostAllowlistService
}

// NewTunnelService 创建隧道服务
func NewTunnelService(db *gorm.DB) *TunnelService {
	return &TunnelService{
		tunnelRepo: repository.NewTunnelRepository(db),
		nodeRepo:   repository.NewNodeRepository(db),
		logService: NewLogService(db),
		allowlist:  NewGostAllowlistService(db),
	}
}

// Create 创建隧道
func (s *TunnelService) Create(req *dto.CreateTunnelReq, username, ip, userAgent string) (*model.GostTunnel, error) {
	endpointTunnelRuntime.Lock()
	defer endpointTunnelRuntime.Unlock()
	_, tunnel, err := s.createGroupConnection(req, username, ip, userAgent)
	return tunnel, err
}

func (s *TunnelService) prepareNewConnection(req *dto.CreateTunnelReq) (*model.GostTunnel, error) {
	nodeIDs, err := s.allowlist.validateNodeIDs(context.Background(), req.AllowlistNodeIDs)
	if err != nil {
		return nil, err
	}
	if req.EntryRuleID != 0 || req.ExitRuleID != 0 {
		if req.EntryNodeID != 0 || req.ExitNodeID != 0 || req.RelayPort != 0 {
			return nil, errors.ErrTunnelEndpointsRequired
		}
		return s.prepareEndpointConnection(req, nodeIDs)
	}
	if req.UseOriginalTarget || len(req.RulesetIDs) != 0 || len(req.ManualDomains) != 0 {
		return nil, errors.ErrTunnelEndpointsRequired
	}
	if err := validateRelayProtocolOptions(req.Protocol, req.ProtocolOptions); err != nil {
		return nil, err
	}
	// 检查入口和出口是否相同
	if req.EntryNodeID == req.ExitNodeID {
		return nil, errors.ErrTunnelNodeSame
	}

	// 检查入口节点是否存在
	entryNode, err := s.nodeRepo.FindByID(req.EntryNodeID)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.ErrEntryNodeNotFound
		}
		return nil, err
	}

	// 检查出口节点是否存在
	exitNode, err := s.nodeRepo.FindByID(req.ExitNodeID)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.ErrExitNodeNotFound
		}
		return nil, err
	}

	// 创建隧道
	tunnel := &model.GostTunnel{
		Name:             req.Name,
		EntryNodeID:      req.EntryNodeID,
		ExitNodeID:       req.ExitNodeID,
		Protocol:         req.Protocol,
		ProtocolOptions:  req.ProtocolOptions,
		RelayPort:        req.RelayPort,
		Remark:           req.Remark,
		Status:           model.TunnelStatusStopped,
		AllowlistNodeIDs: nodeIDs,
		AutoAllowlist:    req.AllowlistNodeIDs == nil,
		EntryNode:        entryNode,
		ExitNode:         exitNode,
	}
	if tunnel.AutoAllowlist {
		tunnel.RelaySecret = rand.Text()
	}
	return tunnel, nil
}

// Update 更新隧道（仅支持更新非运行中的隧道，且不能修改入口/出口节点）
func (s *TunnelService) Update(id uint, req *dto.UpdateTunnelReq, username, ip, userAgent string) (*model.GostTunnel, error) {
	endpointTunnelRuntime.Lock()
	defer endpointTunnelRuntime.Unlock()
	tunnel, err := s.prepareConnectionUpdate(id, req)
	if err != nil {
		return nil, err
	}
	if err := s.tunnelRepo.Update(tunnel, req.AllowlistNodeIDs != nil); err != nil {
		return nil, err
	}
	s.recordTunnelAction(tunnel, model.ActionUpdate, "更新", username, ip, userAgent)
	return tunnel, nil
}

func (s *TunnelService) prepareConnectionUpdate(id uint, req *dto.UpdateTunnelReq) (*model.GostTunnel, error) {
	tunnel, err := s.tunnelRepo.FindByID(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.ErrTunnelNotFound
		}
		return nil, err
	}

	// 检查是否在运行中
	if tunnel.Status == model.TunnelStatusRunning {
		return nil, errors.ErrTunnelRunning
	}
	if req.AllowlistNodeIDs != nil {
		nodeIDs, err := s.allowlist.validateNodeIDs(context.Background(), req.AllowlistNodeIDs)
		if err != nil {
			return nil, err
		}
		tunnel.AllowlistNodeIDs = nodeIDs
		tunnel.AutoAllowlist = false
	}
	previousProtocol := tunnel.Protocol
	if tunnel.EntryRuleID != nil || req.EntryRuleID != 0 || req.ExitRuleID != 0 {
		if tunnel.EntryRuleID == nil || req.RelayPort != 0 {
			return nil, errors.ErrTunnelEndpointsRequired
		}
		if tunnel.Status == model.TunnelStatusError {
			if err := s.stopEndpointTunnel(tunnel); err != nil {
				return nil, err
			}
		}
		if err := s.setTunnelEndpoints(tunnel, req.EntryRuleID, req.ExitRuleID); err != nil {
			return nil, err
		}
		if tunnel.ExitRule.Role != model.EndpointRoleExit && req.Protocol != "" {
			tunnel.Protocol = req.Protocol
		}
		if err := s.setTunnelTargets(tunnel, req.Targets, req.UseOriginalTarget, req.UseClashRuleset, req.RulesetIDs, req.ManualDomains, req.Strategy); err != nil {
			return nil, err
		}
	} else {
		if (req.UseOriginalTarget != nil && *req.UseOriginalTarget) || len(req.RulesetIDs) != 0 || len(req.ManualDomains) != 0 {
			return nil, errors.ErrTunnelEndpointsRequired
		}
		tunnel.Protocol = req.Protocol
		tunnel.RelayPort = req.RelayPort
	}
	if (tunnel.ExitRule == nil || tunnel.ExitRule.Role != model.EndpointRoleExit) &&
		(req.ProtocolOptions != nil || tunnel.Protocol != previousProtocol) {
		if err := validateRelayProtocolOptions(tunnel.Protocol, req.ProtocolOptions); err != nil {
			return nil, err
		}
		tunnel.ProtocolOptions = req.ProtocolOptions
	}

	// 更新隧道（不能修改入口/出口节点）
	tunnel.Name = req.Name
	tunnel.Remark = req.Remark
	return tunnel, nil
}

// UpdateAllowlist 只更新白名单，运行中的隧道会在下一轮同步应用。
func (s *TunnelService) UpdateAllowlist(ctx context.Context, id uint, nodeIDs []uint, username, ip, userAgent string) (*model.GostTunnel, error) {
	tunnel, err := s.GetByID(id)
	if err != nil {
		return nil, err
	}
	nodeIDs, err = s.allowlist.validateNodeIDs(ctx, nodeIDs)
	if err != nil {
		return nil, err
	}
	tunnel.AllowlistNodeIDs = nodeIDs
	tunnel.AutoAllowlist = false
	if err := s.tunnelRepo.UpdateAllowlist(ctx, tunnel); err != nil {
		return nil, err
	}
	s.logService.Record(username, model.ActionUpdate, model.ResourceTypeTunnel, id,
		fmt.Sprintf("更新隧道 IP 白名单: %s", tunnel.Name), ip, userAgent)
	return tunnel, nil
}

// Delete 删除隧道
// 如果有规则正在使用此隧道，不允许删除
func (s *TunnelService) Delete(id uint, username, ip, userAgent string) error {
	endpointTunnelRuntime.Lock()
	defer endpointTunnelRuntime.Unlock()
	tunnel, err := s.tunnelRepo.FindByID(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return errors.ErrTunnelNotFound
		}
		return err
	}

	// 检查是否有规则正在使用此隧道
	hasRules, err := s.tunnelRepo.HasRules(id)
	if err != nil {
		return err
	}
	if hasRules {
		return errors.ErrTunnelHasRules
	}

	// Retained runtime identities also need cleanup after a failed start or node disconnect.
	if tunnel.Status != model.TunnelStatusStopped || tunnel.ServiceID != "" || tunnel.ChainID != "" {
		if err = s.stop(id, username, ip, userAgent); err != nil {
			return err
		}
	}

	// 删除隧道
	if err = s.tunnelRepo.Delete(id); err != nil {
		return err
	}

	s.logService.Record(
		username,
		model.ActionDelete,
		model.ResourceTypeTunnel,
		id,
		fmt.Sprintf("删除隧道: %s", tunnel.Name),
		ip,
		userAgent)

	logger.Infof("删除隧道成功: %s", tunnel.Name)
	return nil
}

// GetByID 获取隧道详情
func (s *TunnelService) GetByID(id uint) (*model.GostTunnel, error) {
	tunnel, err := s.tunnelRepo.FindByID(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.ErrTunnelNotFound
		}
		return nil, err
	}
	if tunnel.Status == model.TunnelStatusRunning {
		tunnel.EntryRule, tunnel.ExitRule = tunnel.EntryRule.WithAppliedEndpoint(), tunnel.ExitRule.WithAppliedEndpoint()
	}
	return tunnel, nil
}

// List 获取隧道列表
func (s *TunnelService) List(req *dto.TunnelListReq) ([]model.GostTunnel, int64, error) {
	req.SetDefaults()

	opt := &repository.QueryOption{
		Pagination: &repository.Pagination{
			Page:     req.Page,
			PageSize: req.PageSize,
		},
		Conditions: make(map[string]any),
	}

	if req.NodeID > 0 {
		opt.Conditions["entry_node_id = ? OR exit_node_id = ?"] = []interface{}{req.NodeID, req.NodeID}
	}
	if req.Status != "" {
		opt.Conditions["status = ?"] = req.Status
	}
	if req.Keyword != "" {
		opt.Conditions["name LIKE ?"] = []interface{}{
			"%" + req.Keyword + "%",
		}
	}

	tunnels, total, err := s.tunnelRepo.List(opt)
	for i := range tunnels {
		if tunnels[i].Status == model.TunnelStatusRunning {
			tunnels[i].EntryRule, tunnels[i].ExitRule = tunnels[i].EntryRule.WithAppliedEndpoint(), tunnels[i].ExitRule.WithAppliedEndpoint()
		}
	}
	return tunnels, total, err
}

// Start 启动隧道
// 在出口节点创建 Relay 服务，在入口节点创建 Chain 连接到出口节点
func (s *TunnelService) Start(id uint, username, ip, userAgent string) error {
	endpointTunnelRuntime.Lock()
	defer endpointTunnelRuntime.Unlock()
	tunnel, err := s.tunnelRepo.FindByID(id)
	if err != nil {
		return err
	}
	if err := s.startConnection(tunnel); err != nil {
		return err
	}
	s.recordTunnelAction(tunnel, model.ActionStart, "启动", username, ip, userAgent)
	return nil
}

func (s *TunnelService) startConnection(tunnel *model.GostTunnel) (err error) {
	// 已在运行中则跳过
	if tunnel.Status == model.TunnelStatusRunning {
		return nil
	}
	if tunnel.EntryRuleID != nil {
		return s.startEndpointTunnel(tunnel)
	}

	// 获取入口和出口节点
	entryNode, err := s.nodeRepo.FindByID(tunnel.EntryNodeID)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return errors.ErrEntryNodeNotFound
		}
		return err
	}
	exitNode, err := s.nodeRepo.FindByID(tunnel.ExitNodeID)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return errors.ErrExitNodeNotFound
		}
		return err
	}

	// 检查节点状态
	if entryNode.Status == model.NodeStatusOffline {
		return errors.ErrEntryNodeOffline
	}
	if exitNode.Status == model.NodeStatusOffline {
		return errors.ErrExitNodeOffline
	}
	tunnel.EntryNode, tunnel.ExitNode = entryNode, exitNode
	if tunnel.Status == model.TunnelStatusError || tunnel.ServiceID != "" || tunnel.ChainID != "" {
		if err := s.removeLegacyTunnelRuntime(tunnel); err != nil {
			return err
		}
	}
	id := tunnel.ID
	relayName, chainName := relayServiceName(id), tunnelChainName(id)
	// Persist deterministic resource names before provisioning, including for a failed rollback.
	if err := s.tunnelRepo.UpdateFields(tunnel, id, map[string]any{
		"service_id": relayName, "chain_id": chainName, "status": model.TunnelStatusError,
	}); err != nil {
		return err
	}
	tunnel.ServiceID, tunnel.ChainID = relayName, chainName
	defer func() {
		if err != nil {
			if cleanupErr := s.removeLegacyTunnelRuntime(tunnel); cleanupErr != nil {
				err = stderrors.Join(err, cleanupErr)
			}
		}
	}()

	// 步骤1：在出口节点创建 Relay 服务
	exitClient := utils.GetGostClient(exitNode)
	var auth *gost.AuthConfig
	if tunnel.RelaySecret != "" {
		auth = &gost.AuthConfig{Username: relayName, Password: tunnel.RelaySecret}
	}

	relaySvc := &gost.ServiceConfig{
		Name:     relayName,
		Addr:     fmt.Sprintf(":%d", tunnel.RelayPort),
		Metadata: map[string]any{"enableStats": true},
		Handler: &gost.HandlerConfig{
			Type: "relay", // relay 协议支持接收客户端传来的目标地址
			Auth: auth,
		},
		Listener: gost.RelayListener(tunnel.Protocol, tunnel.ProtocolOptions),
	}

	if err = s.allowlist.prepareRelay(exitClient, relaySvc, tunnel); err != nil {
		return err
	}
	created, err := createServiceWithTrafficQuota(s.nodeRepo, exitClient, *exitNode, relaySvc)
	if err != nil {
		if stderrors.Is(err, errors.ErrNodeTrafficQuotaUnavailable) {
			return err
		}
		return errors.ErrTunnelRelayCreateFailed
	}
	if !created {
		if _, err = exitClient.SetRelayAllowlist(relaySvc.Name, relaySvc.Admission != ""); err != nil {
			return err
		}
	}

	// 保存出口节点配置
	if err := exitClient.SaveConfig(); err != nil {
		return err
	}

	// 步骤2：在入口节点创建 Chain 连接到出口节点的 Relay 服务
	entryClient := utils.GetGostClient(entryNode)

	// 从出口节点配置中获取主机 IP
	exitHost := exitNode.Address
	if exitHost == "" {
		return errors.ErrExtractHostFailed
	}

	relayAddr := net.JoinHostPort(strings.Trim(exitHost, "[]"), strconv.Itoa(tunnel.RelayPort))

	chain := newTunnelChain(chainName, relayAddr, tunnel.Protocol, tunnel.ProtocolOptions, auth)

	if err = entryClient.CreateChain(chain); err != nil {
		return errors.ErrTunnelChainCreateFailed
	}

	// 保存入口节点配置
	if err := entryClient.SaveConfig(); err != nil {
		return err
	}

	if err := s.tunnelRepo.UpdateStatus(id, model.TunnelStatusRunning); err != nil {
		return err
	}
	logger.Infof("启动隧道成功: %s (Relay: %s -> Chain: %s)", tunnel.Name, relayName, chainName)
	return nil
}

func tunnelChainName(id uint) string {
	return fmt.Sprintf("tunnel-%d-chain", id)
}

func newTunnelChain(name, address, protocol string, options map[string]string, auth *gost.AuthConfig) *gost.ChainConfig {
	return &gost.ChainConfig{Name: name, Hops: []*gost.HopConfig{{Name: "hop-0", Nodes: []*gost.NodeConfig{{
		Name: "exit-relay", Addr: address,
		Connector: &gost.ConnectorConfig{Type: "relay", Auth: auth},
		Dialer:    gost.RelayDialer(protocol, options),
	}}}}}
}

// Stop 停止隧道
// 删除入口节点的 Chain 和出口节点的 Relay 服务
func (s *TunnelService) Stop(id uint, username, ip, userAgent string) error {
	endpointTunnelRuntime.Lock()
	defer endpointTunnelRuntime.Unlock()
	return s.stop(id, username, ip, userAgent)
}

func (s *TunnelService) stop(id uint, username, ip, userAgent string) error {
	tunnel, err := s.tunnelRepo.FindByID(id)
	if err != nil {
		return err
	}
	if err := s.stopConnection(tunnel); err != nil {
		return err
	}
	s.recordTunnelAction(tunnel, model.ActionStop, "停止", username, ip, userAgent)
	return nil
}

func (s *TunnelService) stopConnection(tunnel *model.GostTunnel) error {
	if tunnel.EntryRuleID != nil {
		if tunnel.Status == model.TunnelStatusStopped {
			return nil
		}
		return s.stopEndpointTunnel(tunnel)
	}
	if tunnel.Status == model.TunnelStatusStopped && tunnel.ServiceID == "" && tunnel.ChainID == "" {
		return nil
	}

	if err := s.removeLegacyTunnelRuntime(tunnel); err != nil {
		return err
	}

	if err := s.tunnelRepo.UpdateFields(tunnel, tunnel.ID, map[string]any{
		"service_id": "", "chain_id": "", "status": model.TunnelStatusStopped,
	}); err != nil {
		return err
	}
	logger.Infof("停止隧道成功: %s", tunnel.Name)
	return nil
}

func (s *TunnelService) removeLegacyTunnelRuntime(tunnel *model.GostTunnel) error {
	if tunnel.EntryNode == nil {
		return errors.ErrEntryNodeNotFound
	}
	if tunnel.ExitNode == nil {
		return errors.ErrExitNodeNotFound
	}
	if err := endpointNodesOnline(tunnel); err != nil {
		return err
	}
	entryClient, exitClient := utils.GetGostClient(tunnel.EntryNode), utils.GetGostClient(tunnel.ExitNode)
	chainID, serviceID := tunnel.ChainID, tunnel.ServiceID
	if chainID == "" {
		chainID = tunnelChainName(tunnel.ID)
	}
	if serviceID == "" {
		serviceID = relayServiceName(tunnel.ID)
	}
	if err := stderrors.Join(entryClient.DeleteChain(chainID), entryClient.DeleteChain(clashTunnelChainName(tunnel.ID)), exitClient.DeleteService(serviceID)); err != nil {
		return err
	}
	return stderrors.Join(entryClient.SaveConfig(), exitClient.SaveConfig())
}
