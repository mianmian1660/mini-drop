// ============================================================
// server/group.go — 用户组管理处理器（W5）
// 包含：创建/列表/详情/更新/删除组、添加/移除成员
// ============================================================

package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

// CreateGroupReq 创建组请求
type CreateGroupReq struct {
	Name string `json:"name" binding:"required"`
}

// UpdateGroupReq 更新组请求
type UpdateGroupReq struct {
	Name    string `json:"name"`
	OwnerID string `json:"owner_id"`
}

// AddMemberReq 添加成员请求
type AddMemberReq struct {
	UID string `json:"uid" binding:"required"`
}

// ----------------------------------------------------------
// CreateGroup 创建用户组
// POST /api/v1/groups
// ----------------------------------------------------------
func (s *APIServer) CreateGroup(c *gin.Context) {
	var req CreateGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "参数错误: " + err.Error()})
		return
	}

	uid := c.GetHeader("Drop_user_uid")
	if uid == "" {
		uid = "default-user"
	}

	gid := "grp-" + util.GenTID()[4:] // grp-20260619-a1b2c3d4

	group := &model.Group{
		GID:     gid,
		Name:    req.Name,
		OwnerID: uid,
	}

	if err := s.DB.Create(group).Error; err != nil {
		s.Logger.Error("创建组失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "创建组失败"})
		return
	}

	// 创建者自动成为组成员
	member := &model.GroupMember{GID: gid, UID: uid}
	s.DB.Create(member)

	s.Logger.Info("用户组已创建", zap.String("gid", gid), zap.String("name", req.Name))
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": group})
}

// ----------------------------------------------------------
// ListGroups 获取用户组列表
// GET /api/v1/groups
// ----------------------------------------------------------
func (s *APIServer) ListGroups(c *gin.Context) {
	var groups []model.Group
	if err := s.DB.Order("created_at DESC").Find(&groups).Error; err != nil {
		s.Logger.Error("查询组列表失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "查询失败"})
		return
	}

	if groups == nil {
		groups = []model.Group{}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{"groups": groups, "total": len(groups)},
	})
}

// ----------------------------------------------------------
// GetGroupDetail 获取组详情（含成员列表）
// GET /api/v1/groups/:gid
// ----------------------------------------------------------
func (s *APIServer) GetGroupDetail(c *gin.Context) {
	gid := c.Param("gid")

	var group model.Group
	if err := s.DB.Where("gid = ?", gid).First(&group).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "组不存在: " + gid})
		return
	}

	// 查询组成员
	var members []model.GroupMember
	s.DB.Where("gid = ?", gid).Find(&members)
	if members == nil {
		members = []model.GroupMember{}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"group":   group,
			"members": members,
		},
	})
}

// ----------------------------------------------------------
// UpdateGroup 更新组信息
// PUT /api/v1/groups/:gid
// ----------------------------------------------------------
func (s *APIServer) UpdateGroup(c *gin.Context) {
	gid := c.Param("gid")

	var req UpdateGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "参数错误"})
		return
	}

	updates := map[string]interface{}{}
	if req.Name != "" {
		updates["name"] = req.Name
	}
	if req.OwnerID != "" {
		updates["owner_id"] = req.OwnerID
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "无更新内容"})
		return
	}

	result := s.DB.Model(&model.Group{}).Where("gid = ?", gid).Updates(updates)
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "组不存在: " + gid})
		return
	}

	s.Logger.Info("用户组已更新", zap.String("gid", gid))
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "更新成功"})
}

// ----------------------------------------------------------
// DeleteGroup 删除组
// DELETE /api/v1/groups/:gid
// ----------------------------------------------------------
func (s *APIServer) DeleteGroup(c *gin.Context) {
	gid := c.Param("gid")

	// 删除组成员关系
	s.DB.Where("gid = ?", gid).Delete(&model.GroupMember{})

	// 删除组
	result := s.DB.Where("gid = ?", gid).Delete(&model.Group{})
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "组不存在: " + gid})
		return
	}

	s.Logger.Info("用户组已删除", zap.String("gid", gid))
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "组已删除"})
}

// ----------------------------------------------------------
// AddGroupMember 添加组成员
// POST /api/v1/groups/:gid/members
// ----------------------------------------------------------
func (s *APIServer) AddGroupMember(c *gin.Context) {
	gid := c.Param("gid")

	var req AddMemberReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "参数错误"})
		return
	}

	// 检查组是否存在
	var group model.Group
	if err := s.DB.Where("gid = ?", gid).First(&group).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "组不存在: " + gid})
		return
	}

	// 添加成员（忽略重复）
	member := &model.GroupMember{GID: gid, UID: req.UID}
	if err := s.DB.Where("gid = ? AND uid = ?", gid, req.UID).
		FirstOrCreate(member).Error; err != nil {
		s.Logger.Error("添加成员失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "添加成员失败"})
		return
	}

	s.Logger.Info("组成员已添加", zap.String("gid", gid), zap.String("uid", req.UID))
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "成员已添加"})
}

// ----------------------------------------------------------
// RemoveGroupMember 移除组成员
// DELETE /api/v1/groups/:gid/members/:uid
// ----------------------------------------------------------
func (s *APIServer) RemoveGroupMember(c *gin.Context) {
	gid := c.Param("gid")
	uid := c.Param("uid")

	result := s.DB.Where("gid = ? AND uid = ?", gid, uid).Delete(&model.GroupMember{})
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "成员不存在"})
		return
	}

	s.Logger.Info("组成员已移除", zap.String("gid", gid), zap.String("uid", uid))
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "成员已移除"})
}
