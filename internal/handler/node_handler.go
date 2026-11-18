package handler

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"yagp/internal/dto"
	"yagp/internal/model"
	"yagp/internal/service"
	"yagp/pkg/geoip"
	"yagp/pkg/logger"
	"yagp/pkg/response"

	"github.com/gin-gonic/gin"
)

// NodeHandler 节点控制器
// 处理节点相关的 HTTP 请求
type NodeHandler struct {
	nodeService    *service.NodeService
	upgradeService *service.NodeUpgradeService
	geoIP          *geoip.Database
	geoIPClient    *http.Client
}

// NewNodeHandler 创建节点控制器
func NewNodeHandler(nodeService *service.NodeService, upgradeService *service.NodeUpgradeService, geoIP *geoip.Database) *NodeHandler {
	return &NodeHandler{nodeService: nodeService, upgradeService: upgradeService, geoIP: geoIP, geoIPClient: http.DefaultClient}
}

// Create 创建节点
func (h *NodeHandler) Create(c *gin.Context) {
	var req dto.CreateNodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	ctx := GetRequestContext(c)

	node, err := h.nodeService.Create(&req, ctx.Username, ctx.ClientIP, ctx.UserAgent)
	if err != nil {
		response.HandleError(c, err)
		return
	}

	response.Success(c, node)
}

func (h *NodeHandler) IssueRegistrationToken(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		response.BadRequest(c, "无效的节点 ID")
		return
	}
	ctx := GetRequestContext(c)
	token, err := h.nodeService.IssueRegistrationToken(c.Request.Context(), uint(id), ctx.Username, ctx.ClientIP, ctx.UserAgent)
	if err != nil {
		response.HandleError(c, err)
		return
	}
	response.Success(c, token)
}

func (h *NodeHandler) RevokeCredentials(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		response.BadRequest(c, "无效的节点 ID")
		return
	}
	if err := h.nodeService.RevokeCredentials(c.Request.Context(), uint(id)); err != nil {
		response.HandleError(c, err)
		return
	}
	operator := GetRequestContext(c)
	h.nodeService.RecordOperation(uint(id), "吊销节点通信凭据", operator.Username, operator.ClientIP, operator.UserAgent)
	response.Success(c, nil)
}

// Update 更新节点
func (h *NodeHandler) Update(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		response.BadRequest(c, "无效的节点 ID")
		return
	}

	var req dto.UpdateNodeReq
	if err = c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	ctx := GetRequestContext(c)

	node, err := h.nodeService.Update(uint(id), &req, ctx.Username, ctx.ClientIP, ctx.UserAgent)
	if err != nil {
		response.HandleError(c, err)
		return
	}

	h.setNodeRegions(c.Request.Context(), node)
	response.Success(c, node)
}

// Delete 删除节点
func (h *NodeHandler) Delete(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		response.BadRequest(c, "无效的节点 ID")
		return
	}

	ctx := GetRequestContext(c)

	if err = h.nodeService.Delete(uint(id), ctx.Username, ctx.ClientIP, ctx.UserAgent); err != nil {
		response.HandleError(c, err)
		return
	}

	response.SuccessWithMessage(c, "删除成功", nil)
}

// GetByID 获取节点详情
func (h *NodeHandler) GetByID(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		response.BadRequest(c, "无效的节点 ID")
		return
	}

	node, err := h.nodeService.GetByID(uint(id))
	if err != nil {
		response.HandleError(c, err)
		return
	}

	ctx := c.Request.Context()
	usage, err := h.nodeService.CurrentTrafficUsage(ctx, []model.GostNode{*node}, time.Now())
	if err != nil {
		response.HandleError(c, err)
		return
	}
	h.setNodeRegions(ctx, node)
	response.Success(c, withCurrentTrafficUsage(*node, usage))
}

// List 获取节点列表
func (h *NodeHandler) List(c *gin.Context) {
	var req dto.NodeListReq
	if err := c.ShouldBindQuery(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	nodes, total, err := h.nodeService.List(&req)
	if err != nil {
		response.HandleError(c, err)
		return
	}

	ctx := c.Request.Context()
	usage, err := h.nodeService.CurrentTrafficUsage(ctx, nodes, time.Now())
	if err != nil {
		response.HandleError(c, err)
		return
	}
	items := make([]nodeTrafficResponse, len(nodes))
	regionTargets := make([]*model.GostNode, len(nodes))
	for i, node := range nodes {
		items[i] = withCurrentTrafficUsage(node, usage)
		regionTargets[i] = &items[i].GostNode
	}
	h.setNodeRegions(ctx, regionTargets...)
	response.SuccessPage(c, items, total, req.Page, req.PageSize)
}

func (h *NodeHandler) RoutingOptions(c *gin.Context) {
	var query struct {
		Before uint32 `form:"before"`
	}
	if err := c.ShouldBindQuery(&query); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	options, next, err := h.nodeService.RoutingOptions(c.Request.Context(), uint(query.Before))
	if err != nil {
		response.HandleError(c, err)
		return
	}
	response.Success(c, gin.H{"list": options, "next": next})
}

// GetConfig 获取节点配置
func (h *NodeHandler) GetConfig(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		response.BadRequest(c, "无效的节点 ID")
		return
	}

	config, err := h.nodeService.GetConfig(uint(id))
	if err != nil {
		response.HandleError(c, err)
		return
	}

	response.Success(c, config)
}

func (h *NodeHandler) setNodeRegions(ctx context.Context, nodes ...*model.GostNode) {
	if h.geoIP == nil {
		return
	}
	var fallbackCtx context.Context
	for _, node := range nodes {
		for _, address := range []string{node.IPv4, node.IPv6} {
			ip, err := netip.ParseAddr(address)
			if err != nil {
				continue
			}
			var code string
			if fallbackCtx == nil {
				code, err = h.geoIP.Country(ctx, ip)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					logger.Warnf("GeoIP 数据库查询失败，回退到旧版 GeoJS 在线查询: %v", err)
					// One time budget covers online lookups for the whole response.
					var cancel context.CancelFunc
					fallbackCtx, cancel = context.WithTimeout(ctx, 3*time.Second)
					defer cancel()
				}
			}
			if fallbackCtx != nil {
				if fallbackCtx.Err() != nil {
					return
				}
				ip = ip.Unmap()
				if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Zone() != "" {
					continue
				}
				code = geoip.Country(fallbackCtx, h.geoIPClient, ip)
			}
			if code != "" {
				node.Region = code
				break
			}
		}
	}
}

type nodeTrafficResponse struct {
	model.GostNode
	CurrentPeriodUsedBytes *uint64 `json:"traffic_current_period_used_bytes"`
}

func withCurrentTrafficUsage(node model.GostNode, usage map[uint]uint64) nodeTrafficResponse {
	result := nodeTrafficResponse{GostNode: node}
	if bytes, ok := usage[node.ID]; ok {
		result.CurrentPeriodUsedBytes = &bytes
	}
	return result
}

func (h *NodeHandler) CalibrateTraffic(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		response.BadRequest(c, "无效的节点 ID")
		return
	}
	var request struct {
		InputBytes     *int64  `json:"input_bytes"`
		OutputBytes    *int64  `json:"output_bytes"`
		QuotaUsedBytes *uint64 `json:"quota_used_bytes"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "流量必须为整数个字节")
		return
	}
	ctx := c.Request.Context()
	var details string
	if request.QuotaUsedBytes != nil {
		if request.InputBytes != nil || request.OutputBytes != nil || *request.QuotaUsedBytes > math.MaxInt64 {
			response.BadRequest(c, "请单独校准配额用量，并填写有效的字节数")
			return
		}
		err = h.nodeService.CalibrateNodeQuota(ctx, uint(id), *request.QuotaUsedBytes)
		details = fmt.Sprintf("校准当前周期配额用量为 %d 字节", *request.QuotaUsedBytes)
	} else {
		if request.InputBytes == nil || request.OutputBytes == nil || *request.InputBytes < 0 || *request.OutputBytes < 0 ||
			*request.InputBytes > math.MaxInt64-*request.OutputBytes {
			response.BadRequest(c, "请填写有效的累计入站和出站字节数")
			return
		}
		err = h.nodeService.CalibrateNodeTotals(ctx, uint(id), *request.InputBytes, *request.OutputBytes)
		details = fmt.Sprintf("校准累计流量：入站 %d 字节，出站 %d 字节", *request.InputBytes, *request.OutputBytes)
	}
	if err != nil {
		nodeOperationError(c, err)
		return
	}
	h.recordOperation(c, uint(id), details)
	response.SuccessWithMessage(c, "流量已校准", nil)
}
