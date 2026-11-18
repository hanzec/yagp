//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// For an override, render YAML to stdout so the installer can validate a staged copy.
func installListenAddress(path, addr, port string) (string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	if port == "current" {
		return addr, nil
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return "", fmt.Errorf("无效的监听端口 %q", port)
	}
	newAddr := net.JoinHostPort(host, strconv.FormatUint(number, 10))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if addr == newAddr {
		return string(data), nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", err
	}
	if len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	server, err := installConfigField(doc.Content[0], "server")
	if err != nil {
		return "", err
	}
	value, err := installConfigField(server, "port")
	if err != nil {
		return "", err
	}
	value.SetString(newAddr)
	data, err = yaml.Marshal(&doc)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func installConfigField(mapping *yaml.Node, key string) (*yaml.Node, error) {
	switch {
	case mapping.Kind == yaml.AliasNode:
		// Override the inherited value locally without changing other users of the anchor.
		alias := *mapping
		*mapping = yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, &alias}}
	case mapping.Kind == 0 || mapping.ShortTag() == "!!null":
		*mapping = yaml.Node{Kind: yaml.MappingNode}
	case mapping.Kind != yaml.MappingNode:
		return nil, fmt.Errorf("无法在 YAML 配置中设置 %s", key)
	}
	for i := 0; i < len(mapping.Content); i += 2 {
		if strings.EqualFold(mapping.Content[i].Value, key) {
			return mapping.Content[i+1], nil
		}
	}
	value := new(yaml.Node)
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, value)
	return value, nil
}
