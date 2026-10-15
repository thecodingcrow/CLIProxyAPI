package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelusage"
)

// GetModelUsage returns per-provider/per-model token usage over the recent-request window.
func (h *Handler) GetModelUsage(c *gin.Context) {
	c.JSON(http.StatusOK, modelusage.Default().Snapshot())
}
