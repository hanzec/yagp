package handler

import (
	"fmt"
	"time"

	"yagp/pkg/nodegrpc"
	"yagp/pkg/response"

	"github.com/gin-gonic/gin"
)

type nodeNftConfig struct {
	Config string `json:"config"`
	Table  string `json:"table"`
	Live   bool   `json:"live"`
}

type nodeFirewallStatus struct {
	Supported      bool       `json:"supported"`
	Managed        bool       `json:"managed"`
	Synchronized   bool       `json:"synchronized"`
	LastSync       *time.Time `json:"last_sync"`
	LastError      *string    `json:"last_error"`
	Backend        string     `json:"backend"`
	DisabledReason string     `json:"disabled_reason,omitempty"`
}

func (h *NodeHandler) GetFirewallStatus(c *gin.Context) {
	client, _ := h.nodeClient(c)
	if client == nil {
		return
	}
	state, err := client.GetFirewallStatus(c.Request.Context())
	if err != nil {
		nodeOperationError(c, err)
		return
	}
	response.Success(c, firewallStatusResponse(state))
}

func (h *NodeHandler) EnableFirewall(c *gin.Context) {
	client, id := h.nodeClient(c)
	if client == nil {
		return
	}
	state, err := client.EnableFirewall(c.Request.Context())
	if err != nil {
		nodeOperationError(c, err)
		return
	}
	h.recordOperation(c, id, "启用自动防火墙规则")
	response.Success(c, firewallStatusResponse(state))
}

func firewallStatusResponse(state *nodegrpc.FirewallStatus) nodeFirewallStatus {
	result := nodeFirewallStatus{
		Supported: state.GetSupported(), Managed: state.GetManaged(), Synchronized: state.GetSynchronized(),
		LastError: state.LastError, Backend: state.GetBackend(), DisabledReason: state.GetDisabledReason(),
	}
	if timestamp := state.GetLastSync(); timestamp != nil && timestamp.IsValid() {
		lastSync := timestamp.AsTime()
		result.LastSync = &lastSync
	}
	return result
}

func nftTemplate(table string) string {
	return fmt.Sprintf(`{
  "nftables": [
    {
      "add": {
        "chain": {
          "family": "inet",
          "table": "%s",
          "name": "prerouting",
          "type": "nat",
          "hook": "prerouting",
          "prio": -100,
          "policy": "accept"
        }
      }
    }
  ]
}`, table)
}

func (h *NodeHandler) GetNftConfig(c *gin.Context) {
	client, id := h.nodeClient(c)
	if client == nil {
		return
	}
	config, err := client.GetNftConfig(c.Request.Context())
	if err != nil {
		nodeOperationError(c, err)
		return
	}
	table := config.GetTable()
	if table != nodegrpc.SingleNodeNftTable {
		table = nodegrpc.NftTableName(uint64(id))
	}
	current := config.GetLiveConfig()
	live := current != ""
	if !live {
		current = config.GetConfig()
	}
	response.Success(c, nodeNftConfig{Config: current, Table: table, Live: live})
}
