package service

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"yagp/internal/config"
	"yagp/internal/errors"
	"yagp/internal/repository"
	"yagp/pkg/logger"

	"gorm.io/gorm"
)

const backupTimeLayout = "20060102_150405"

// Older snapshots have whole-second timestamps. CreateTemp adds the decimal
// suffix on current snapshots to avoid collisions at the same nanosecond.
var backupNamePattern = regexp.MustCompile(`^gost_panel_([0-9]{8}_[0-9]{6})(?:\.[0-9]{9}-[0-9]+)?\.db$`)

// BackupService 备份服务
type BackupService struct {
	db *gorm.DB
}

func NewBackupService(db *gorm.DB) *BackupService {
	return &BackupService{db: db}
}

func (s *BackupService) Supported() bool {
	return s.db.Dialector.Name() == "sqlite"
}

// Run checks for today's snapshot at startup and once per hour until shutdown.
func (s *BackupService) Run(ctx context.Context) {
	if !s.Supported() {
		return
	}
	logger.Info("自动备份服务已启动")
	defer logger.Info("自动备份服务已停止")
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for ctx.Err() == nil {
		s.processAutoBackup(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// processAutoBackup 执行自动备份逻辑
func (s *BackupService) processAutoBackup(ctx context.Context) {
	cfg, err := repository.NewSystemConfigRepository(s.db.WithContext(ctx)).Get()
	if err != nil {
		logger.Errorf("自动备份检查失败: 获取系统配置错误: %v", err)
		return
	}

	if !cfg.AutoBackup {
		return
	}

	backupDir := "backups"
	// 确保目录存在
	if err = os.MkdirAll(backupDir, 0755); err != nil {
		logger.Errorf("自动备份失败: 无法创建目录: %v", err)
		return
	}

	today := "gost_panel_" + time.Now().Format("20060102") + "_"
	stems, err := publishedBackupStems(backupDir)
	if err != nil {
		logger.Errorf("自动备份失败: 读取目录错误: %v", err)
		return
	}
	for _, stem := range stems {
		if strings.HasPrefix(stem, today) {
			return
		}
	}

	// 执行备份
	logger.Infof("开始执行自动备份...")
	if err := s.CreateBackup(ctx); err != nil && ctx.Err() == nil {
		logger.Errorf("自动备份执行失败: %v", err)
	}
}

// CreateBackup 创建备份
func (s *BackupService) CreateBackup(ctx context.Context) error {
	if !s.Supported() {
		return errors.ErrBackupUnsupported
	}

	// 获取数据库路径
	dbPath := config.Get().Database.Path
	if dbPath == "" {
		return errors.ErrDBPathNotConfigured
	}

	// 确保备份目录存在
	backupDir := "backups"
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return errors.ErrBackupDirCreateFailed
	}

	// Keep incomplete snapshots out of retention and daily-backup checks. A unique
	// temporary name also lets manual and scheduled backups run in the same second.
	prefix := "gost_panel_" + time.Now().Format(backupTimeLayout+".000000000")
	temporary, err := os.CreateTemp(backupDir, "."+prefix+"-*.tmp")
	if err != nil {
		return errors.ErrBackupFailed
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Close(); err != nil {
		return errors.ErrBackupFailed
	}
	// Copying the main database file would omit committed data still in its WAL.
	err = s.db.WithContext(ctx).Exec("VACUUM INTO ?", temporaryPath).Error
	if cancelErr := ctx.Err(); cancelErr != nil {
		return cancelErr
	}
	if err != nil {
		logger.Errorf("VACUUM INTO 备份失败: %v", err)
		return errors.ErrBackupFailed
	}
	filename := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(temporaryPath), "."), ".tmp") + ".db"
	targetPath := filepath.Join(backupDir, filename)
	if err := os.Rename(temporaryPath, targetPath); err != nil {
		return errors.ErrBackupFailed
	}

	logger.Infof("数据库备份成功: %s", targetPath)

	// 清理旧备份
	s.cleanOldBackups(ctx, backupDir)

	return nil
}

// A stem sorts chronologically even when legacy and fractional timestamps share a second.
// The .db extension would put a legacy snapshot after its newer fractional peers.
func publishedBackupStems(backupDir string) ([]string, error) {
	dir, err := os.Open(backupDir)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var stems []string
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		parts := backupNamePattern.FindStringSubmatch(entry.Name())
		if parts == nil {
			continue
		}
		if _, err := time.Parse(backupTimeLayout, parts[1]); err == nil {
			stems = append(stems, strings.TrimSuffix(entry.Name(), ".db"))
		}
	}
	return stems, nil
}

// cleanOldBackups 清理旧备份
func (s *BackupService) cleanOldBackups(ctx context.Context, backupDir string) {
	cfg, err := repository.NewSystemConfigRepository(s.db.WithContext(ctx)).Get()
	if err != nil {
		logger.Errorf("获取系统配置失败: %v", err)
		return
	}

	retentionCount := cfg.BackupRetentionCount
	if retentionCount <= 0 {
		return
	}

	stems, err := publishedBackupStems(backupDir)
	if err != nil {
		logger.Errorf("读取备份目录失败: %v", err)
		return
	}
	if len(stems) <= retentionCount {
		return
	}
	slices.Sort(stems)
	for _, stem := range stems[:len(stems)-retentionCount] {
		filePath := filepath.Join(backupDir, stem+".db")
		if err := os.Remove(filePath); err != nil {
			logger.Errorf("删除旧备份失败 %s: %v", filePath, err)
		} else {
			logger.Infof("已删除旧备份: %s", filePath)
		}
	}
}
