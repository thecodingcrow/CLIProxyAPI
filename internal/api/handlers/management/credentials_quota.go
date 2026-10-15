package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
)

// SetClaudeQuota installs the Claude plan-limit reader.
func (h *Handler) SetClaudeQuota(reader claudequota.Reader) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.claudeQuota = reader
	h.mu.Unlock()
}

// GetCredentialsQuota serves per-credential Claude plan limits derived from
// observed rate-limit headers, and their summary. It only reads memory.
func (h *Handler) GetCredentialsQuota(c *gin.Context) {
	h.mu.Lock()
	reader := h.claudeQuota
	h.mu.Unlock()
	if reader == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "claude quota cache unavailable"})
		return
	}
	c.JSON(http.StatusOK, reader.Snapshot())
}
