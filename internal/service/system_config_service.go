package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"

	"yagp/internal/buildinfo"
	"yagp/internal/config"
	"yagp/internal/dto"
	"yagp/internal/errors"
	"yagp/internal/repository"
	"yagp/pkg/upgrade"
)

// SystemConfigService 系统配置服务
type SystemConfigService struct {
	repo *repository.SystemConfigRepository
}

// NewSystemConfigService 创建系统配置服务
func NewSystemConfigService(repo *repository.SystemConfigRepository) *SystemConfigService {
	return &SystemConfigService{repo: repo}
}

// GetConfig 获取配置DTO
func (s *SystemConfigService) GetConfig() (*dto.SystemConfigResp, error) {
	config, err := s.repo.Get()
	if err != nil {
		return nil, err
	}

	return &dto.SystemConfigResp{
		Email: dto.EmailConfigResp{
			Host:      config.SMTPHost,
			Port:      config.SMTPPort,
			Username:  config.SMTPUsername,
			Password:  config.SMTPPassword,
			FromEmail: config.SMTPFrom,
		},
		Bark: dto.BarkConfig{
			ServerURL: config.BarkServerURL,
			DeviceKey: config.BarkDeviceKey,
		},
		Config: dto.PanelSettingResp{
			SiteTitle: config.SiteTitle,
			LogoURL:   config.LogoURL,
			Copyright: config.Copyright,
		},
		Log: dto.LogConfigResp{
			RetentionDays: config.LogRetentionDays,
			Level:         config.LogLevel,
		},
		Backup: dto.BackupConfigResp{
			AutoBackup:     config.AutoBackup,
			RetentionCount: config.BackupRetentionCount,
		},
	}, nil
}

// GetPublicConfig 获取公开配置
func (s *SystemConfigService) GetPublicConfig() (*dto.PublicSystemConfigResp, error) {
	site, err := s.repo.Get()
	if err != nil {
		return nil, err
	}

	public := &dto.PublicSystemConfigResp{
		SiteTitle: site.SiteTitle,
		LogoURL:   site.LogoURL,
		Copyright: site.Copyright,
		Version:   buildinfo.Version,
	}
	if cfg := config.Get(); cfg.Report.Listen != "" {
		public.Host = cfg.Host
	}
	if upgrade.ValidateTag("daemon", buildinfo.Version) {
		public.DaemonVersion = buildinfo.Version
	}
	return public, nil
}

// UpdateConfig 更新配置
func (s *SystemConfigService) UpdateConfig(req *dto.UpdateSystemConfigReq) error {
	var bark dto.BarkConfig
	if req.Bark != nil {
		bark = *req.Bark
		if _, err := normalizeBarkConfig(&bark, true); err != nil {
			return err
		}
	}

	config, err := s.repo.Get()
	if err != nil {
		return err
	}

	// 映射 Email
	config.SMTPHost = req.Email.Host
	config.SMTPPort = req.Email.Port
	config.SMTPUsername = req.Email.Username
	config.SMTPPassword = req.Email.Password
	config.SMTPFrom = req.Email.FromEmail

	// 映射 Config
	config.SiteTitle = req.Config.SiteTitle
	config.LogoURL = req.Config.LogoURL
	config.Copyright = req.Config.Copyright

	// 映射 Log
	config.LogRetentionDays = req.Log.RetentionDays
	config.LogLevel = req.Log.Level

	// 映射 Backup
	config.AutoBackup = req.Backup.AutoBackup
	config.BackupRetentionCount = req.Backup.RetentionCount

	if req.Bark == nil {
		return s.repo.Update(config, "BarkServerURL", "BarkDeviceKey")
	}
	config.BarkServerURL = bark.ServerURL
	config.BarkDeviceKey = bark.DeviceKey
	if err := s.repo.Update(config); err != nil {
		return errors.ErrOperationFailed
	}
	return nil
}

const maxBarkResponseBytes = 4 << 10

var barkHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		// Do not forward the device key to another endpoint.
		return http.ErrUseLastResponse
	},
}

// SendTestBark sends a test notification using the submitted configuration.
func (s *SystemConfigService) SendTestBark(ctx context.Context, config dto.BarkConfig) error {
	server, err := normalizeBarkConfig(&config, false)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(struct {
		DeviceKey string `json:"device_key"`
		Title     string `json:"title"`
		Body      string `json:"body"`
	}{
		DeviceKey: config.DeviceKey,
		Title:     "yagp 测试通知",
		Body:      "这是一条来自 yagp 的 Bark 测试推送。",
	})
	if err != nil {
		return errors.ErrInternal
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.JoinPath("push").String(), bytes.NewReader(payload))
	if err != nil {
		return errors.ErrBarkServerURLInvalid
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := barkHTTPClient.Do(req)
	if err != nil {
		return errors.ErrBarkPushFailed
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.ErrBarkPushFailed
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBarkResponseBytes+1))
	if err != nil || len(body) > maxBarkResponseBytes {
		return errors.ErrBarkPushFailed
	}
	var result struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Code != http.StatusOK {
		return errors.ErrBarkPushFailed
	}
	return nil
}

func normalizeBarkConfig(config *dto.BarkConfig, allowEmpty bool) (*url.URL, error) {
	config.ServerURL = strings.TrimSpace(config.ServerURL)
	config.DeviceKey = strings.TrimSpace(config.DeviceKey)
	if allowEmpty && config.ServerURL == "" && config.DeviceKey == "" {
		return nil, nil
	}
	if config.ServerURL == "" || config.DeviceKey == "" {
		return nil, errors.ErrBarkConfigIncomplete
	}
	if len(config.DeviceKey) > 255 {
		return nil, errors.ErrBarkDeviceKeyInvalid
	}

	server, err := url.Parse(config.ServerURL)
	if err != nil || len(config.ServerURL) > 2048 || (server.Scheme != "http" && server.Scheme != "https") ||
		server.Hostname() == "" || server.User != nil || server.RawQuery != "" || server.ForceQuery ||
		strings.Contains(config.ServerURL, "#") {
		return nil, errors.ErrBarkServerURLInvalid
	}
	if port := server.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return nil, errors.ErrBarkServerURLInvalid
		}
	}
	return server, nil
}

// SendTestEmail 发送测试邮件
func (s *SystemConfigService) SendTestEmail(ctx context.Context, req *dto.EmailConfigReq) error {
	// 验证必要参数
	if req.Host == "" || req.Port == 0 || req.FromEmail == "" {
		return errors.ErrSMTPConfigIncomplete
	}

	// Leave time for the HTTP response within the panel's 10-second request timeout.
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	addr := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	var conn net.Conn
	var err error
	if req.Port == 465 {
		dialer := tls.Dialer{Config: smtpTLSConfig(req.Host)}
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	} else {
		var dialer net.Dialer
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return errors.ErrSMTPConnectFailed
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return errors.ErrSMTPConnectFailed
	}
	// net/smtp has no context parameter. Interrupt pending I/O if the browser disconnects.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	toEmail := req.FromEmail
	if req.ToEmail != "" {
		toEmail = req.ToEmail
	}

	msg := []byte(fmt.Sprintf("To: %s\r\n"+
		"Subject: yagp 测试邮件\r\n"+
		"\r\n"+
		"这是一封来自 yagp 的测试邮件。\r\n"+
		"如果您收到这封邮件，说明您的 SMTP 配置正确。\r\n", toEmail))

	return sendSMTPTestMessage(conn, req, toEmail, msg)
}

func smtpTLSConfig(host string) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
}

func sendSMTPTestMessage(conn net.Conn, req *dto.EmailConfigReq, recipient string, msg []byte) error {
	client, err := smtp.NewClient(conn, req.Host)
	if err != nil {
		return errors.ErrSMTPClientFailed
	}
	if req.Port != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(smtpTLSConfig(req.Host)); err != nil {
				return errors.ErrSMTPConnectFailed
			}
		}
	}
	if req.Username != "" || req.Password != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			return errors.ErrSMTPAuthFailed
		}
		auth := smtp.PlainAuth("", req.Username, req.Password, req.Host)
		if err := client.Auth(auth); err != nil {
			return errors.ErrSMTPAuthFailed
		}
	}
	if err := client.Mail(req.FromEmail); err != nil {
		return errors.ErrSMTPSenderFailed
	}
	if err := client.Rcpt(recipient); err != nil {
		return errors.ErrSMTPRecipientFailed
	}
	w, err := client.Data()
	if err != nil {
		return errors.ErrSMTPDataFailed
	}
	if _, err := w.Write(msg); err != nil {
		return errors.ErrSMTPWriteFailed
	}
	if err := w.Close(); err != nil {
		return errors.ErrSMTPCloseFailed
	}
	return client.Quit()
}
