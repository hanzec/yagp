//go:build linux

package nodebridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"yagp/pkg/nodegrpc"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"gopkg.in/yaml.v3"
)

const (
	PanelCredentialsFile = "panel-credentials.yaml"
	panelConfigCache     = "panel-config.pb"
	refreshLeadTime      = time.Minute
)

func NewPanelClient(host nodegrpc.PanelHost, caFile, fingerprint string) (*http.Client, error) {
	tlsConfig, err := host.TLSConfig(caFile, fingerprint)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSClientConfig:       tlsConfig,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}, nil
}

type panelCredentials struct {
	AccessToken      string `yaml:"access_token"`
	RefreshToken     string `yaml:"refresh_token"`
	AccessExpiresAt  int64  `yaml:"access_expires_at"`
	RefreshExpiresAt int64  `yaml:"refresh_expires_at"`
	PendingRefreshID string `yaml:"pending_refresh_id,omitempty"`
}

func parsePanelCredentials(reply *nodegrpc.NodeCredentials) (panelCredentials, error) {
	credentials := panelCredentials{
		AccessToken: reply.GetAccessToken(), RefreshToken: reply.GetRefreshToken(),
		AccessExpiresAt: reply.GetAccessExpiresAt(), RefreshExpiresAt: reply.GetRefreshExpiresAt(),
	}
	if !credentials.valid() || credentials.AccessExpiresAt <= time.Now().Unix() {
		return panelCredentials{}, errors.New("panel returned invalid credentials")
	}
	return credentials, nil
}

func (c panelCredentials) valid() bool {
	return c.AccessToken != "" && c.RefreshToken != "" && c.AccessExpiresAt > 0 && c.RefreshExpiresAt > c.AccessExpiresAt
}

func EncodePanelCredentials(reply *nodegrpc.NodeCredentials) ([]byte, error) {
	credentials, err := parsePanelCredentials(reply)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(credentials)
}

func writePanelCredentials(path string, credentials panelCredentials) error {
	data, err := yaml.Marshal(credentials)
	if err != nil {
		return err
	}
	return WriteNodeFile(path, data)
}

// WriteNodeFile atomically replaces one private node state file.
func WriteNodeFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".yagp-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

type credentialManager struct {
	mu          sync.Mutex
	credentials panelCredentials
	path        string
	nodeID      uint64
	host        nodegrpc.PanelHost
	client      *http.Client
}

func openCredentialManager(cfg Config, host nodegrpc.PanelHost, client *http.Client) (*credentialManager, error) {
	path := filepath.Join(filepath.Dir(cfg.ConfigFile), PanelCredentialsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read panel credentials: %w", err)
	}
	var credentials panelCredentials
	if err = yaml.Unmarshal(data, &credentials); err != nil || !credentials.valid() {
		return nil, errors.New("invalid stored panel credentials")
	}
	return &credentialManager{credentials: credentials, path: path, nodeID: cfg.NodeID, host: host, client: client}, nil
}

func (m *credentialManager) accessToken(ctx context.Context, lead time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.credentials.PendingRefreshID != "" || m.credentials.AccessExpiresAt <= time.Now().Add(lead).Unix() {
		if err := m.refresh(ctx); err != nil {
			return "", err
		}
	}
	return m.credentials.AccessToken, nil
}

func (m *credentialManager) nextRefresh() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.credentials.PendingRefreshID != "" {
		return 0
	}
	return time.Until(time.Unix(m.credentials.AccessExpiresAt, 0).Add(-refreshLeadTime))
}

// Persist the ID before sending it so retries after a lost reply or restart are identical.
func (m *credentialManager) refresh(ctx context.Context) error {
	if m.credentials.PendingRefreshID == "" {
		if m.credentials.RefreshExpiresAt <= time.Now().Unix() {
			return errors.New("panel refresh token expired; re-enroll the node")
		}
		var nonce [16]byte
		_, _ = rand.Read(nonce[:])
		pending := m.credentials
		pending.PendingRefreshID = hex.EncodeToString(nonce[:])
		if err := writePanelCredentials(m.path, pending); err != nil {
			return fmt.Errorf("persist refresh request: %w", err)
		}
		m.credentials = pending
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request := &nodegrpc.RefreshNodeRequest{
		NodeId: m.nodeID, RefreshToken: m.credentials.RefreshToken, RequestId: m.credentials.PendingRefreshID,
	}
	reply := new(nodegrpc.NodeCredentials)
	if err := nodegrpc.PostProtobuf(ctx, m.client, m.host.HTTPURL(nodegrpc.RefreshPath), request, reply); err != nil {
		var response *nodegrpc.HTTPStatusError
		if errors.As(err, &response) && response.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("panel rejected the refresh token; re-enroll the node: %w", err)
		}
		return fmt.Errorf("refresh panel credentials: %w", err)
	}
	credentials, err := parsePanelCredentials(reply)
	if err != nil {
		return err
	}
	if err = writePanelCredentials(m.path, credentials); err != nil {
		return fmt.Errorf("persist refreshed panel credentials: %w", err)
	}
	m.credentials = credentials
	return nil
}

func maintainCredentials(ctx context.Context, credentials *credentialManager) {
	for delay := credentials.nextRefresh(); ctx.Err() == nil; {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if _, err := credentials.accessToken(ctx, refreshLeadTime); err != nil {
			if ctx.Err() == nil {
				log.Printf("could not refresh panel credentials: %v", err)
			}
			delay = 30 * time.Second
			continue
		}
		delay = credentials.nextRefresh()
	}
}

func maintainPanelChannel(ctx context.Context, credentials *credentialManager, configPath string, current *atomic.Pointer[nodegrpc.Session], handler nodegrpc.StreamHandler, firewallChanged chan<- struct{}) {
	for delay := time.Second; ctx.Err() == nil; delay = min(delay*2, 30*time.Second) {
		connected, err := runPanelSession(ctx, credentials, configPath, current, handler, firewallChanged)
		if ctx.Err() != nil {
			return
		}
		if connected {
			delay = time.Second
		} else if err != nil {
			log.Printf("panel connection attempt failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func runPanelSession(ctx context.Context, credentials *credentialManager, configPath string, current *atomic.Pointer[nodegrpc.Session], handler nodegrpc.StreamHandler, firewallChanged chan<- struct{}) (bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	accessToken, err := credentials.accessToken(ctx, 0)
	if err != nil {
		return false, err
	}
	stream, err := credentials.host.DialSession(ctx, credentials.client, credentials.nodeID, accessToken)
	if err != nil {
		return false, err
	}
	defer stream.Close()

	session := nodegrpc.NewSession(stream, handler)
	defer session.Close()
	client := nodegrpc.NewV1Client(session)
	done := make(chan error, 1)
	go func() { done <- session.Run() }()
	if err := fetchPanelConfig(ctx, client, configPath); err != nil {
		return false, fmt.Errorf("load panel configuration: %w", err)
	}
	current.Store(session)
	defer current.Store(nil)
	wakeFirewallWorker(firewallChanged)
	ipTicks := time.NewTicker(5 * time.Minute)
	defer ipTicks.Stop()
	go reportIPChanges(ctx, credentials.nodeID, client, detectNodeIPs, ipTicks.C)
	return true, <-done
}

func fetchPanelConfig(ctx context.Context, client nodegrpc.V1Client, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	config, err := client.GetNodeConfig(ctx, &emptypb.Empty{})
	if err != nil {
		return err
	}
	return storePanelConfig(path, config)
}

func storePanelConfig(configPath string, config *nodegrpc.NodeConfig) error {
	data, err := proto.Marshal(config)
	if err != nil {
		return err
	}
	return WriteNodeFile(filepath.Join(filepath.Dir(configPath), panelConfigCache), data)
}

func readPanelConfig(configPath string) (*nodegrpc.NodeConfig, error) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), panelConfigCache))
	if err != nil {
		return nil, fmt.Errorf("read node configuration cached by the daemon: %w", err)
	}
	config := new(nodegrpc.NodeConfig)
	if err = proto.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("decode cached node configuration: %w", err)
	}
	return config, nil
}
