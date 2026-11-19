//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"yagp/internal/buildinfo"
	"yagp/internal/config"
	"yagp/internal/model"
	"yagp/internal/repository"
	"yagp/internal/router"
	"yagp/internal/service"
	"yagp/pkg/geoip"
	"yagp/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func main() {
	// 解析命令行参数
	var configPath string
	flag.StringVar(&configPath, "c", "", "配置文件路径")
	flag.StringVar(&configPath, "config", "", "配置文件路径")
	showVersion := flag.Bool("version", false, "打印构建版本")
	installPort := flag.String("install-port", "", "传入 current 打印监听地址，传入端口号输出更新后的 YAML 配置")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildinfo.Version)
		return
	}

	// 加载配置
	cfg, err := config.Load(configPath)
	if err != nil {
		panic("加载配置失败: " + err.Error())
	}

	if *installPort != "" {
		output, err := installListenAddress(configPath, cfg.Server.Port, *installPort)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(strings.TrimSuffix(output, "\n"))
		return
	}

	// 初始化日志
	if err = logger.Init(&logger.Config{
		Level:  cfg.Log.Level,
		Format: cfg.Log.Format,
		Output: cfg.Log.Output,
	}); err != nil {
		panic("初始化日志失败: " + err.Error())
	}
	defer func() {
		_ = logger.Sync()
	}()

	logger.Info("yagp 启动中...")
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	backgroundCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()

	// 设置 Gin 模式
	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	// 初始化数据库
	db, err := initDatabase(cfg)
	if err != nil {
		logger.Fatalf("初始化数据库失败: %v", err)
	}
	logger.Info("数据库初始化完成")

	// 自动迁移
	if err = autoMigrate(db); err != nil {
		logger.Fatalf("数据库迁移失败: %v", err)
	}
	logger.Info("数据库迁移完成")

	// 初始化系统配置
	if err = initSystemConfig(db); err != nil {
		logger.Fatalf("初始化系统配置失败: %v", err)
	}

	reportServer, reportFingerprint, err := startReports(cfg.Report, cfg.Host, db)
	if err != nil {
		logger.Fatalf("启动流量上报服务失败: %v", err)
	}
	if reportServer != nil {
		defer reportServer.Close()
	}

	engine := gin.New()
	if err = engine.SetTrustedProxies(cfg.Authentik.TrustedProxies); err != nil {
		logger.Fatalf("反向代理配置无效: %v", err)
	}

	nodeUpgrade := service.NewNodeUpgradeService(db)
	geoIP := startGeoIP(backgroundCtx, geoip.DefaultDatabasePath, cfg.GeoIP.DownloadURL)
	r, err := router.NewRouter(db, cfg.Authentik, nodeUpgrade, reportFingerprint, geoIP)
	if err != nil {
		logger.Fatalf("Authentik 配置无效: %v", err)
	}
	r.Setup(engine)

	// 启动服务器
	server := &http.Server{Addr: cfg.Server.Port, Handler: engine.Handler()}
	go func() {
		logger.Infof("服务器启动在 %s", cfg.Server.Port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("服务器启动失败: %v", err)
		}
	}()

	// 同步节点健康状态和转发规则
	syncService := service.NewRuleSyncService(db)
	syncService.Start()
	go service.NewGostRulesetService(db).Run(backgroundCtx)
	go nodeUpgrade.Run(backgroundCtx)
	go service.NewTunnelService(db).RunMonitor(backgroundCtx)

	// 启动自动备份服务
	backupService := service.NewBackupService(db)
	var backupWorker sync.WaitGroup
	backupWorker.Go(func() { backupService.Run(backgroundCtx) })

	<-quit

	logger.Info("yagp 正在关闭...")
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancelDrain()
	if err := server.Shutdown(drainCtx); err != nil {
		logger.Warnf("等待 HTTP 请求结束失败: %v", err)
		_ = server.Close()
	}

	// 停止相关的后台服务
	stopBackground()
	syncService.Stop()
	backupWorker.Wait()
}

// initDatabase 初始化数据库
func initDatabase(cfg *config.Config) (*gorm.DB, error) {
	// 配置 GORM 日志
	var gormLogLevel gormlogger.LogLevel
	switch cfg.Log.Level {
	case "debug":
		gormLogLevel = gormlogger.Info
	case "info":
		gormLogLevel = gormlogger.Warn
	default:
		gormLogLevel = gormlogger.Error
	}

	gormConfig := &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormLogLevel),
	}

	switch strings.ToLower(strings.TrimSpace(cfg.Database.Type)) {
	case "sqlite":
		return gorm.Open(sqlite.Open(cfg.Database.Path), gormConfig)
	case "mysql":
		if strings.TrimSpace(cfg.Database.DSN) == "" {
			return nil, fmt.Errorf("MySQL 需要配置 database.dsn 或 YAGP_DATABASE_DSN")
		}
		dsn, err := mysqldriver.ParseDSN(cfg.Database.DSN)
		if err != nil || dsn.DBName == "" {
			return nil, fmt.Errorf("MySQL 连接串无效或未指定数据库名")
		}
		dsn.ParseTime = true
		return gorm.Open(mysql.New(mysql.Config{DSNConfig: dsn}), gormConfig)
	default:
		return nil, fmt.Errorf("不支持的数据库类型 %q，请使用 sqlite 或 mysql", cfg.Database.Type)
	}
}

// autoMigrate 自动迁移数据库表结构
func autoMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&model.GostNode{},
		&model.GostRule{},
		&model.ExitRelayHistory{},
		&model.GostTunnel{},
		&model.TunnelGroup{},
		&model.TunnelProbeSample{},
		&model.GostRuleset{},
		&model.OperationLog{},
		&model.SystemConfig{},
	); err != nil {
		return err
	}

	if err := migrateTunnelAllowlists(db); err != nil {
		return err
	}
	if err := migrateTunnelEndpointRoles(db); err != nil {
		return err
	}
	return repository.MigrateTunnelGroups(db)
}

func migrateTunnelAllowlists(db *gorm.DB) error {
	const legacyTable = "gost_allowlists"
	if !db.Migrator().HasTable(legacyTable) {
		return nil
	}

	// An explicit empty list also marks a tunnel as migrated.
	legacySelection := db.Table(legacyTable).Select("node_ids").Where("id = ?", 1)
	return db.Model(&model.GostTunnel{}).Where("allowlist_node_ids IS NULL").
		UpdateColumn("allowlist_node_ids", gorm.Expr("COALESCE((?), ?)", legacySelection, "[]")).Error
}

// initSystemConfig 初始化系统配置
func initSystemConfig(db *gorm.DB) error {
	const upstreamURL = "https://github.com/code-gopher/gostPanel"

	var count int64
	if err := db.Model(&model.SystemConfig{}).Count(&count).Error; err != nil {
		return err
	}

	if count != 0 {
		return nil
	}

	sysConfig := &model.SystemConfig{
		SiteTitle:            "yagp",
		Copyright:            upstreamURL,
		LogRetentionDays:     7,
		LogLevel:             "info",
		AutoBackup:           false,
		BackupRetentionCount: 7,
	}
	if err := db.Create(sysConfig).Error; err != nil {
		return err
	}
	logger.Info("初始化默认系统配置完成")
	return nil
}

func startGeoIP(ctx context.Context, path, downloadURL string) *geoip.Database {
	db := geoip.NewDatabase(path, downloadURL)
	if err := db.Load(); err != nil {
		logger.Warnf("GeoIP 本地数据库不可用，暂时使用在线查询: %s: %v", path, err)
	} else {
		logger.Infof("GeoIP 本地数据库已加载: %s", path)
	}

	logger.Infof("GeoIP 数据库后台下载开始: %s", path)
	go func() {
		if err := db.Refresh(ctx); err != nil {
			if ctx.Err() != nil {
				logger.Infof("GeoIP 数据库后台下载已取消: %s", path)
			} else if db.Ready() {
				logger.Warnf("GeoIP 数据库下载更新失败，继续使用本地缓存: %s: %v", path, err)
			} else {
				logger.Warnf("GeoIP 数据库下载更新失败，使用在线查询: %s: %v", path, err)
			}
			return
		}
		logger.Infof("GeoIP 数据库下载更新成功: %s", path)
	}()
	return db
}
