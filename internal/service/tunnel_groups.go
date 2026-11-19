package service

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"yagp/internal/dto"
	"yagp/internal/errors"
	"yagp/internal/model"
	"yagp/internal/repository"

	"gorm.io/gorm"
)

type TunnelGroupView struct {
	*model.GostTunnel                    // Preserve the connection fields for single-connection clients.
	ID                uint               `json:"id"`
	Name              string             `json:"name"`
	Remark            string             `json:"remark"`
	EntryNodeID       uint               `json:"entry_node_id"`
	ExitNodeID        uint               `json:"exit_node_id"`
	EntryNode         *model.GostNode    `json:"entry_node,omitempty"`
	ExitNode          *model.GostNode    `json:"exit_node,omitempty"`
	Status            model.TunnelStatus `json:"status"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
	Connections       []model.GostTunnel `json:"connections"`
}

func tunnelGroupView(group *model.TunnelGroup) *TunnelGroupView {
	for i := range group.Connections {
		connection := &group.Connections[i]
		if connection.Status == model.TunnelStatusRunning {
			connection.EntryRule = connection.EntryRule.WithAppliedEndpoint()
			connection.ExitRule = connection.ExitRule.WithAppliedEndpoint()
		}
	}
	view := &TunnelGroupView{
		ID: group.ID, Name: group.Name, Remark: group.Remark, Status: group.Status,
		EntryNodeID: group.EntryNodeID, ExitNodeID: group.ExitNodeID, CreatedAt: group.CreatedAt, UpdatedAt: group.UpdatedAt,
		Connections: group.Connections,
	}
	for _, connection := range group.Connections {
		if connection.ID == group.ID {
			view.EntryNode, view.ExitNode = connection.EntryNode, connection.ExitNode
			break
		}
	}
	if len(group.Connections) == 1 {
		view.GostTunnel = &group.Connections[0]
	}
	return view
}

func (s *TunnelService) findGroup(id uint, acceptConnectionID bool) (*model.TunnelGroup, error) {
	group, err := s.tunnelRepo.FindGroup(id, acceptConnectionID)
	if stderrors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errors.ErrTunnelNotFound
	}
	return group, err
}

func (s *TunnelService) GetGroup(id uint) (*TunnelGroupView, error) {
	group, err := s.findGroup(id, true)
	if err != nil {
		return nil, err
	}
	return tunnelGroupView(group), nil
}

func (s *TunnelService) ListGroups(req *dto.TunnelListReq) ([]*TunnelGroupView, int64, error) {
	req.SetDefaults()
	conditions := make(map[string]any)
	if req.NodeID > 0 {
		connections := s.tunnelRepo.DB.Model(&model.GostTunnel{}).Select("group_id").
			Where("entry_node_id = ? OR exit_node_id = ?", req.NodeID, req.NodeID)
		conditions["tunnel_groups.id IN (?)"] = connections
	}
	if req.Status != "" {
		conditions["state.status = ?"] = req.Status
	}
	if keyword := strings.TrimSpace(req.Keyword); keyword != "" {
		like := "%" + keyword + "%"
		conditions["tunnel_groups.name LIKE ? OR tunnel_groups.remark LIKE ?"] = []any{like, like}
	}
	groups, total, err := s.tunnelRepo.ListGroups(&repository.QueryOption{
		Conditions: conditions,
		Pagination: &repository.Pagination{Page: req.Page, PageSize: req.PageSize},
	})
	if err != nil {
		return nil, 0, err
	}
	views := make([]*TunnelGroupView, 0, len(groups))
	for i := range groups {
		views = append(views, tunnelGroupView(&groups[i]))
	}
	return views, total, nil
}

func (s *TunnelService) CreateGroup(req *dto.CreateTunnelReq, user, ip, agent string) (*TunnelGroupView, error) {
	endpointTunnelRuntime.Lock()
	defer endpointTunnelRuntime.Unlock()
	group, _, err := s.createGroupConnection(req, user, ip, agent)
	if err != nil {
		return nil, err
	}
	return s.GetGroup(group.ID)
}

func (s *TunnelService) createGroupConnection(req *dto.CreateTunnelReq, user, ip, agent string) (*model.TunnelGroup, *model.GostTunnel, error) {
	connection, err := s.prepareNewConnection(req)
	if err != nil {
		return nil, nil, err
	}
	matches, err := s.tunnelRepo.MatchingGroups(connection.EntryNodeID, connection.ExitNodeID, 0)
	if err != nil {
		return nil, nil, err
	}
	newGroup := len(matches) == 0
	start := !newGroup || connection.EntryRuleID != nil
	for _, existing := range matches {
		start = start && existing.Status == model.TunnelStatusRunning
	}
	if start {
		if err := endpointNodesOnline(connection); err != nil {
			return nil, nil, err
		}
	}
	group, err := s.tunnelRepo.AddGroupConnection(connection, matches)
	if err != nil {
		return nil, nil, err
	}
	if start {
		if err := s.startConnection(connection); err != nil {
			// A merged tunnel keeps its connecting path for retry. Also retain pending remote cleanup.
			if len(matches) > 1 || connection.Status == model.TunnelStatusError ||
				(connection.EntryRuleID == nil && (connection.ServiceID != "" || connection.ChainID != "")) {
				return nil, nil, err
			}
			if newGroup {
				return nil, nil, combineTunnelErrors(err, s.tunnelRepo.DeleteGroup(group))
			}
			return nil, nil, combineTunnelErrors(err, s.tunnelRepo.Delete(connection.ID))
		}
		connection, err = s.tunnelRepo.FindByID(connection.ID)
		if err != nil {
			return nil, nil, err
		}
	}
	if newGroup {
		s.recordGroupAction(group, model.ActionCreate, "创建隧道", user, ip, agent)
	} else {
		s.recordGroupAction(group, model.ActionUpdate, "添加隧道连接", user, ip, agent)
	}
	return group, connection, nil
}

func combineTunnelErrors(actionErr, cleanupErr error) error {
	if cleanupErr != nil {
		return stderrors.Join(actionErr, cleanupErr)
	}
	return actionErr
}

func (s *TunnelService) UpdateGroup(id uint, req *dto.UpdateTunnelReq, user, ip, agent string) (*TunnelGroupView, error) {
	endpointTunnelRuntime.Lock()
	defer endpointTunnelRuntime.Unlock()
	group, err := s.findGroup(id, false)
	if err != nil {
		return nil, err
	}
	var connection *model.GostTunnel
	if changesConnection(req) {
		if len(group.Connections) != 1 {
			return nil, errors.ErrTunnelMultipleConnections
		}
		current := &group.Connections[0]
		if current.EntryRuleID == nil && req.EntryRuleID == 0 && (req.Protocol == "" || req.RelayPort == 0) {
			return nil, errors.ErrTunnelRelayConfigRequired
		}
		connection, err = s.prepareConnectionUpdate(current.ID, req)
		if err != nil {
			return nil, err
		}
		if group.EntryNodeID != connection.EntryNodeID || group.ExitNodeID != connection.ExitNodeID {
			matches, err := s.tunnelRepo.MatchingGroups(connection.EntryNodeID, connection.ExitNodeID, group.ID)
			if err != nil {
				return nil, err
			}
			if len(matches) != 0 {
				if err := s.tunnelRepo.MergeGroup(group, connection, req.AllowlistNodeIDs != nil, matches); err != nil {
					return nil, err
				}
				target := &matches[0]
				s.recordGroupAction(target, model.ActionUpdate, "合并隧道", user, ip, agent)
				return s.GetGroup(target.ID)
			}
			group.EntryNodeID, group.ExitNodeID = connection.EntryNodeID, connection.ExitNodeID
		}
	}
	group.Name, group.Remark = req.Name, req.Remark
	if err := s.tunnelRepo.SaveGroup(group, connection, req.AllowlistNodeIDs != nil); err != nil {
		return nil, err
	}
	s.recordGroupAction(group, model.ActionUpdate, "更新隧道", user, ip, agent)
	return s.GetGroup(group.ID)
}

func changesConnection(req *dto.UpdateTunnelReq) bool {
	return req.EntryRuleID != 0 || req.ExitRuleID != 0 || req.Protocol != "" || req.RelayPort != 0 ||
		req.ProtocolOptions != nil || req.AllowlistNodeIDs != nil || req.Targets != nil || req.UseClashRuleset ||
		req.UseOriginalTarget != nil || req.RulesetIDs != nil || req.ManualDomains != nil || req.Strategy != ""
}

func (s *TunnelService) UpdateGroupAllowlist(ctx context.Context, id uint, nodeIDs []uint, user, ip, agent string) (*TunnelGroupView, error) {
	endpointTunnelRuntime.Lock()
	defer endpointTunnelRuntime.Unlock()
	group, err := s.findGroup(id, false)
	if err != nil {
		return nil, err
	}
	nodeIDs, err = s.allowlist.validateNodeIDs(ctx, nodeIDs)
	if err != nil {
		return nil, err
	}
	if err := s.tunnelRepo.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repo := repository.NewTunnelRepository(tx)
		for i := range group.Connections {
			connection := &group.Connections[i]
			connection.AllowlistNodeIDs, connection.AutoAllowlist = nodeIDs, false
			if err := repo.UpdateAllowlist(ctx, connection); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	s.recordGroupAction(group, model.ActionUpdate, "更新隧道 IP 白名单", user, ip, agent)
	return tunnelGroupView(group), nil
}

func (s *TunnelService) DeleteGroup(id uint, user, ip, agent string) error {
	endpointTunnelRuntime.Lock()
	defer endpointTunnelRuntime.Unlock()
	group, err := s.findGroup(id, false)
	if err != nil {
		return err
	}
	for _, connection := range group.Connections {
		hasRules, err := s.tunnelRepo.HasRules(connection.ID)
		if err != nil {
			return err
		}
		if hasRules {
			return errors.ErrTunnelHasRules
		}
	}
	if err := s.changeConnections(group, s.stopConnection); err != nil {
		return err
	}
	if err := s.tunnelRepo.DeleteGroup(group); err != nil {
		return err
	}
	s.recordGroupAction(group, model.ActionDelete, "删除隧道", user, ip, agent)
	return nil
}

func (s *TunnelService) StartGroup(id uint, user, ip, agent string) error {
	return s.changeGroup(id, s.startConnection, model.ActionStart, "启动隧道", user, ip, agent)
}

func (s *TunnelService) StopGroup(id uint, user, ip, agent string) error {
	return s.changeGroup(id, s.stopConnection, model.ActionStop, "停止隧道", user, ip, agent)
}

func (s *TunnelService) changeGroup(id uint, change func(*model.GostTunnel) error, action, label, user, ip, agent string) error {
	endpointTunnelRuntime.Lock()
	defer endpointTunnelRuntime.Unlock()
	group, err := s.findGroup(id, false)
	if err != nil {
		return err
	}
	if err := s.changeConnections(group, change); err != nil {
		return err
	}
	s.recordGroupAction(group, action, label, user, ip, agent)
	return nil
}

func (s *TunnelService) changeConnections(group *model.TunnelGroup, change func(*model.GostTunnel) error) error {
	for _, connection := range group.Connections {
		// Starting or stopping a peer may change the canonical runtime of a shared listener.
		fresh, err := s.tunnelRepo.FindByID(connection.ID)
		if err != nil {
			return err
		}
		if err := change(fresh); err != nil {
			return err
		}
	}
	return nil
}

func (s *TunnelService) recordGroupAction(group *model.TunnelGroup, action, label, user, ip, agent string) {
	s.logService.Record(user, action, model.ResourceTypeTunnel, group.ID, fmt.Sprintf("%s: %s", label, group.Name), ip, agent)
}
