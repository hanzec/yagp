package gost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"yagp/pkg/nodegrpc"
)

const (
	TransparentBypassName = "yagp-transparent-proxy"
	TunnelProbeBypassName = "yagp-tunnel-probe"
)

type bypassConfig struct {
	Name      string   `json:"name"`
	Whitelist bool     `json:"whitelist"`
	Network   string   `json:"network,omitempty"`
	Matchers  []string `json:"matchers"`
}

func (c *Client) SetTransparentBypass(matchers []string) (bool, error) {
	return c.SetNamedTransparentBypass(TransparentBypassName, matchers)
}

// SetNamedTransparentBypass updates the whitelist referenced by a transparent chain.
func (c *Client) SetNamedTransparentBypass(name string, matchers []string) (bool, error) {
	return c.ensureBypass(bypassConfig{Name: name, Whitelist: true, Matchers: matchers})
}

// EnsureTunnelProbeBypass allows only the daemon's Unix socket through a filtered Relay.
func (c *Client) EnsureTunnelProbeBypass() error {
	_, err := c.ensureBypass(bypassConfig{
		Name: TunnelProbeBypassName, Whitelist: true, Network: "unix",
		Matchers: []string{nodegrpc.TunnelProbeDomain},
	})
	return err
}

func (c *Client) ensureBypass(desired bypassConfig) (bool, error) {
	path := "/config/bypasses/" + desired.Name
	current, err := c.resource(path)
	if err != nil {
		return false, err
	}
	method := http.MethodPost
	if len(current) == 0 {
		path = "/config/bypasses"
	} else {
		var existing bypassConfig
		if err = json.Unmarshal(current, &existing); err != nil {
			return false, err
		}
		if existing.Whitelist == desired.Whitelist && existing.Network == desired.Network && slices.Equal(existing.Matchers, desired.Matchers) {
			return false, nil
		}
		method = http.MethodPut
	}
	resp, err := c.doRequest(method, path, desired)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return false, fmt.Errorf("Gost 分流白名单更新失败（HTTP %d）", resp.StatusCode)
	}
	return true, nil
}

// DeleteNamedTransparentBypass removes a tunnel's whitelist, if present.
func (c *Client) DeleteNamedTransparentBypass(name string) error {
	path := "/config/bypasses/" + name
	current, err := c.resource(path)
	if err != nil || len(current) == 0 {
		return err
	}
	resp, err := c.doRequest(http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("删除 Gost 分流白名单失败（HTTP %d）", resp.StatusCode)
	}
	return nil
}
