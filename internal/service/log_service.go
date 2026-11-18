// Package service 提供业务逻辑层服务
package service

import (
	"yagp/internal/dto"
	"yagp/internal/model"
	"yagp/internal/repository"
	"yagp/pkg/logger"

	"gorm.io/gorm"
)

// LogService 日志服务
// 负责操作日志的查询
type LogService struct {
	logRepo *repository.OperationLogRepository
}

// NewLogService 创建日志服务
func NewLogService(db *gorm.DB) *LogService {
	return &LogService{
		logRepo: repository.NewOperationLogRepository(db),
	}
}

// List 获取日志列表
func (s *LogService) List(req *dto.LogListReq) ([]model.OperationLog, int64, error) {
	// 设置默认值
	req.SetDefaults()

	opt := &repository.QueryOption{
		Pagination: &repository.Pagination{
			Page:     req.Page,
			PageSize: req.PageSize,
		},
		Conditions: make(map[string]any),
	}

	if req.Action != "" {
		opt.Conditions["action = ?"] = req.Action
	}
	if req.ResourceType != "" {
		opt.Conditions["resource_type = ?"] = req.ResourceType
	}
	if req.Username != "" {
		opt.Conditions["username LIKE ?"] = "%" + req.Username + "%"
	}

	return s.logRepo.List(opt)
}

// Record 记录操作日志
func (s *LogService) Record(username, action, resourceType string, resourceID uint, details, ip, userAgent string) {
	log := &model.OperationLog{
		Username:     username,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Details:      details,
		IPAddress:    ip,
		UserAgent:    userAgent,
	}
	if err := s.logRepo.Create(log); err != nil {
		logger.Errorf("记录操作日志失败: %v", err)
	}
}
