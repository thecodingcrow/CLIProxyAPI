package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelusage"
)

// GetModelUsage returns per-provider/per-model token usage over the requested range (default: live window).
func (h *Handler) GetModelUsage(c *gin.Context) {
	r, err := modelusage.ParseRange(c.Query("range"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, modelusage.Default().Snapshot(r))
}
