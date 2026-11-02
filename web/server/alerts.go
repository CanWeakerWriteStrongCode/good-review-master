package server

import (
	"net/http"
	"strconv"

	"good-review-master/logutil"

	"github.com/gin-gonic/gin"
)

// AlertDetail 面板上的一条报警。
//
// Kind 是写库时定的分类（window_too_small、dead_letter_<stage>），
// Detail 是当时的完整现场 JSON（可能几 KB），页面默认折叠，点开才看。
type AlertDetail struct {
	ID            int64  `json:"id"`
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"` // 账号已不在运行时为空，页面显示 ID
	Kind          string `json:"kind"`
	Detail        string `json:"detail"`
	CreatedAt     int64  `json:"created_at"`
	CreatedAtText string `json:"created_at_text"`
}

// 一次返回多少条报警。报警是异常现场不是流水，翻太多也没人看。
const (
	alertLimitDefault = 50
	alertLimitMax     = 200
)

// handleAPIAlerts 返回最近的报警，按时间倒序。
//
// state.db 打不开（纯内存运行）时返回空列表而不是报错：面板上「没有报警」
// 和「读不到报警」是两回事，但为了不把整页拖黄，这里统一按空列表处理，
// 真正的读取失败才回 500。
func (s *Server) handleAPIAlerts() gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := alertLimitDefault
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= alertLimitMax {
				limit = n
			}
		}

		store := s.stateStore()
		list, total, err := store.LoadAlerts(limit)
		if err != nil {
			logutil.Warn("读取报警失败", "err", err)
			c.JSON(http.StatusInternalServerError, APIResponse{Code: 500, Data: gin.H{"msg": "读取报警失败"}})
			return
		}

		names := s.workspaceNames()
		alerts := make([]AlertDetail, 0, len(list))
		for _, item := range list {
			alerts = append(alerts, AlertDetail{
				ID:            item.ID,
				WorkspaceID:   item.WorkspaceID,
				WorkspaceName: names[item.WorkspaceID],
				Kind:          item.Kind,
				Detail:        item.Detail,
				CreatedAt:     item.CreatedAt,
				CreatedAtText: formatTimestamp(item.CreatedAt),
			})
		}

		c.JSON(http.StatusOK, APIResponse{Code: 200, Data: gin.H{
			"alerts": alerts,
			"total":  total,
		}})
	}
}

// workspaceNames 返回账号 ID → 展示名，给报警列表补账号名。
// 账号被删掉后库里的报警还在，查不到名字就留空，页面退回显示 ID。
func (s *Server) workspaceNames() map[string]string {
	views := s.workspaceViews()
	out := make(map[string]string, len(views))
	for _, view := range views {
		out[view.ID] = view.Name
	}
	return out
}
