package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/model"
)

const (
	RoleViewer        = "Viewer"
	RoleOperator      = "Operator"
	RoleGroupAdmin    = "GroupAdmin"
	RolePlatformAdmin = "PlatformAdmin"
)

type AuthContext struct {
	UID    string
	Name   string
	Role   string
	Groups []string
}

func (s *APIServer) AuthContext(c *gin.Context) AuthContext {
	role := readHeaderOrCookie(c, "Drop-User-Role", "Drop_user_role", "drop_user_role")
	if role == "" {
		role = RoleOperator
	}
	switch strings.ToLower(role) {
	case "viewer":
		role = RoleViewer
	case "operator":
		role = RoleOperator
	case "groupadmin", "group_admin":
		role = RoleGroupAdmin
	case "platformadmin", "platform_admin", "admin":
		role = RolePlatformAdmin
	default:
		role = RoleOperator
	}
	return AuthContext{
		UID:    getRequestUID(c),
		Name:   getRequestUserName(c),
		Role:   role,
		Groups: parseCSV(readHeaderOrCookie(c, "Drop-User-Groups", "Drop_user_groups", "drop_user_groups")),
	}
}

func readHeaderOrCookie(c *gin.Context, h1, h2, cookie string) string {
	if c == nil {
		return ""
	}
	for _, h := range []string{h1, h2} {
		if v := strings.TrimSpace(c.GetHeader(h)); v != "" {
			return v
		}
	}
	if v, err := c.Cookie(cookie); err == nil {
		return strings.TrimSpace(v)
	}
	return ""
}

func parseCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		v := strings.TrimSpace(part)
		if v != "" && !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	return out
}

func (a AuthContext) CanWrite() bool {
	return a.Role == RoleOperator || a.Role == RoleGroupAdmin || a.Role == RolePlatformAdmin
}

func (a AuthContext) IsPlatformAdmin() bool {
	return a.Role == RolePlatformAdmin
}

func (a AuthContext) GroupSet() map[string]bool {
	set := map[string]bool{}
	for _, g := range a.Groups {
		set[g] = true
	}
	return set
}

func (s *APIServer) forbid(c *gin.Context) {
	s.RespondHTTPError(c, http.StatusForbidden, ErrCodeAuthForbidden, "没有权限执行此操作")
}

func (s *APIServer) ownerSharesGroup(ownerUID string, auth AuthContext) bool {
	if ownerUID == "" || len(auth.Groups) == 0 {
		return false
	}
	var count int64
	err := s.DB.Model(&model.GroupMember{}).
		Where("uid = ? AND gid IN ?", ownerUID, auth.Groups).
		Count(&count).Error
	return err == nil && count > 0
}

func (s *APIServer) canReadOwner(ownerUID string, auth AuthContext) bool {
	return true
}

func (s *APIServer) canManageOwner(ownerUID string, auth AuthContext) bool {
	if auth.IsPlatformAdmin() || ownerUID == auth.UID && auth.CanWrite() {
		return true
	}
	return auth.Role == RoleGroupAdmin && s.ownerSharesGroup(ownerUID, auth)
}

func (s *APIServer) visibleOwnerUIDs(auth AuthContext) []string {
	seen := map[string]bool{}
	out := []string{}
	if auth.UID != "" {
		seen[auth.UID] = true
		out = append(out, auth.UID)
	}
	if len(auth.Groups) == 0 {
		return out
	}
	var members []model.GroupMember
	if err := s.DB.Where("gid IN ?", auth.Groups).Find(&members).Error; err != nil {
		return out
	}
	for _, member := range members {
		if member.UID != "" && !seen[member.UID] {
			out = append(out, member.UID)
			seen[member.UID] = true
		}
	}
	return out
}

func (s *APIServer) canReadGroup(group model.Group, auth AuthContext) bool {
	if auth.IsPlatformAdmin() || group.OwnerID == auth.UID {
		return true
	}
	for _, gid := range auth.Groups {
		if gid == group.GID {
			return true
		}
	}
	var count int64
	err := s.DB.Model(&model.GroupMember{}).Where("gid = ? AND uid = ?", group.GID, auth.UID).Count(&count).Error
	return err == nil && count > 0
}

func (s *APIServer) canManageGroup(group model.Group, auth AuthContext) bool {
	if auth.IsPlatformAdmin() || group.OwnerID == auth.UID {
		return true
	}
	if auth.Role != RoleGroupAdmin {
		return false
	}
	for _, gid := range auth.Groups {
		if gid == group.GID {
			return true
		}
	}
	return false
}

func (s *APIServer) canReadAgent(agent model.AgentInfo, auth AuthContext) bool {
	return true
}
