package nodebridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"yagp/pkg/nodegrpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"gopkg.in/yaml.v3"
)

func (s *controlServer) EnableFirewall(ctx context.Context, _ *emptypb.Empty) (*nodegrpc.FirewallStatus, error) {
	if runtime.GOOS != "linux" || s.config.ConfigFile == "" {
		return nil, status.Error(codes.FailedPrecondition, "此节点不支持远程启用自动放行")
	}
	s.firewallMu.Lock()
	defer s.firewallMu.Unlock()

	path, err := filepath.EvalSymlinks(s.config.ConfigFile)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "找不到节点实际使用的配置文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "无法读取节点配置文件")
	}
	cfg, updated, err := enableFirewallYAML(data, s.config.NodeID)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	manager, err := s.enabledFirewallManager(cfg)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if updated != nil {
		if err := WriteNodeFile(path, updated); err != nil {
			return nil, status.Error(codes.Internal, "无法保存节点配置文件")
		}
	}
	s.firewall = manager
	select {
	case s.firewallChanged <- struct{}{}:
	default:
	}
	return firewallStatusReply(manager.Status()), nil
}

// An already running manager keeps its current delegation and synchronization status.
func (s *controlServer) enabledFirewallManager(cfg FirewallConfig) (*FirewallManager, error) {
	if current := s.firewall; current != nil && current.Status().DisabledReason == "" {
		if cfg.Table != current.config.Table || cfg.InputChain != current.config.InputChain || cfg.Policy != current.config.Policy {
			return nil, errors.New("节点防火墙配置已变更，请重启守护程序以加载新的放行后端配置")
		}
		return current, nil
	}
	manager, err := NewFirewallManager(s.config.NodeID, cfg)
	if err != nil {
		return nil, err
	}
	state := manager.Status()
	if !state.Supported && state.LastError != nil {
		return nil, errors.New(*state.LastError)
	}
	return manager, nil
}

func enableFirewallYAML(data []byte, nodeID uint64) (FirewallConfig, []byte, error) {
	var cfg FirewallConfig
	var stored struct {
		NodeID   uint64    `yaml:"node_id"`
		Firewall yaml.Node `yaml:"firewall"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document, extra yaml.Node
	if err := decoder.Decode(&document); err != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return cfg, nil, errors.New("节点配置文件必须包含一个 YAML 映射")
	}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return cfg, nil, errors.New("节点配置文件必须只包含一个 YAML 文档")
	}
	if err := document.Decode(&stored); err != nil || stored.NodeID != nodeID {
		return cfg, nil, errors.New("节点配置文件无效或与当前节点不匹配")
	}
	if err := stored.Firewall.Decode(&cfg); err != nil {
		return cfg, nil, errors.New("节点防火墙配置无效")
	}
	if cfg.Enabled != nil && *cfg.Enabled {
		return cfg, nil, nil
	}
	enabledValue := true
	cfg.Enabled = &enabledValue
	root := document.Content[0]
	firewall := yamlField(root, "firewall")
	if firewall != nil && hasYAMLAlias(root, firewall) {
		return cfg, nil, errors.New("节点防火墙配置被其他 YAML 字段引用，无法单独修改")
	}
	if firewall == nil {
		if !stored.Firewall.IsZero() {
			return cfg, nil, errors.New("节点通过顶层 YAML 合并继承防火墙配置，请先将 firewall 配置写到顶层")
		}
		firewall = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, yamlKey("firewall"), firewall)
	} else if firewall.Kind != yaml.MappingNode {
		// Keep the original mapping referenced by a firewall alias unchanged.
		if firewall.Kind == yaml.AliasNode && firewall.Alias.Kind == yaml.MappingNode {
			alias := &yaml.Node{Kind: yaml.AliasNode, Value: firewall.Value, Alias: firewall.Alias}
			firewall.Content = []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, alias}
		}
		firewall.Kind, firewall.Style, firewall.Tag, firewall.Value = yaml.MappingNode, 0, "!!map", ""
		firewall.Alias = nil
	}
	if enabled := yamlField(firewall, "enabled"); enabled == nil {
		firewall.Content = append(firewall.Content, yamlKey("enabled"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	} else {
		if hasYAMLAlias(root, enabled) {
			return cfg, nil, errors.New("节点防火墙开关被其他 YAML 字段引用，无法单独修改")
		}
		enabled.Kind, enabled.Style, enabled.Tag, enabled.Value = yaml.ScalarNode, 0, "!!bool", "true"
		enabled.Alias, enabled.Content = nil, nil
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := errors.Join(encoder.Encode(&document), encoder.Close()); err != nil {
		return cfg, nil, errors.New("无法生成节点配置")
	}
	return cfg, output.Bytes(), nil
}

func yamlKey(name string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}
}

func yamlField(mapping *yaml.Node, name string) *yaml.Node {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == name {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func hasYAMLAlias(node, target *yaml.Node) bool {
	if target.Anchor == "" {
		return false
	}
	if node.Kind == yaml.AliasNode && node.Alias == target {
		return true
	}
	for _, child := range node.Content {
		if hasYAMLAlias(child, target) {
			return true
		}
	}
	return false
}
