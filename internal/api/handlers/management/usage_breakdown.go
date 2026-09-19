package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagestats"
)

// GetUsageBreakdown returns aggregated usage metrics grouped by dimension.
func (h *Handler) GetUsageBreakdown(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	dimension := strings.ToLower(strings.TrimSpace(c.Query("dimension")))
	switch dimension {
	case "model", "endpoint", "provider", "account":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid dimension, expected model, endpoint, provider, or account"})
		return
	}

	to := time.Now().UTC()
	from := to.Add(-24 * time.Hour)

	var err error
	if raw := c.Query("from"); raw != "" {
		from, err = parseUsageTime(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from: " + err.Error()})
			return
		}
	}
	if raw := c.Query("to"); raw != "" {
		to, err = parseUsageTime(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid to: " + err.Error()})
			return
		}
	}

	if c.Query("from") != "" && c.Query("to") == "" {
		to = from.Add(24 * time.Hour)
	} else if c.Query("from") == "" && c.Query("to") != "" {
		from = to.Add(-24 * time.Hour)
	}

	if from.After(to) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "from must not be after to"})
		return
	}

	rows, err := usagestats.QueryBreakdown(usagestats.Dir(h.logDirectory()), from, to, dimension, usagestats.Filter{
		Provider: c.Query("provider"),
		Model:    c.Query("model"),
		Account:  c.Query("account"),
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"from":      from.Format(time.RFC3339Nano),
		"to":        to.Format(time.RFC3339Nano),
		"dimension": dimension,
		"rows":      rows,
	})
}
