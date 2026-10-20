package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelusage"
)

// GetModelUsage returns per-provider/per-model token usage over the requested range (default: live window),
// with claude rows priced against the pooled plan quota when the quota store runs (not in Home mode).
func (h *Handler) GetModelUsage(c *gin.Context) {
	r, err := modelusage.ParseRange(c.Query("range"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.mu.Lock()
	reader := h.claudeQuota
	h.mu.Unlock()
	var pool *claudequota.Pool
	if reader != nil {
		p := reader.Snapshot().Pool()
		pool = &p
	}
	c.JSON(http.StatusOK, modelusage.Default().SnapshotWithQuota(r, pool))
}
