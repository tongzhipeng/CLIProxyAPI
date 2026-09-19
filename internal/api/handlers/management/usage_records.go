package management

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagestats"
)

// GetUsageRecords returns reverse-chronological per-request usage records.
func (h *Handler) GetUsageRecords(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1).Add(-time.Nanosecond)
	if raw := c.Query("date"); raw != "" {
		date, err := time.Parse(usageStatsDateLayout, strings.TrimSpace(raw))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid date, expected YYYY-MM-DD"})
			return
		}
		from = date.UTC()
		to = from.AddDate(0, 0, 1).Add(-time.Nanosecond)
	}

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
	if from.After(to) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "from must not be after to"})
		return
	}

	limit, err := parseNonNegativeInt(c.Query("limit"), 50)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
		return
	}
	offset, err := parseNonNegativeInt(c.Query("offset"), 0)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid offset"})
		return
	}

	var failed *bool
	if raw := c.Query("failed"); raw != "" {
		value, errParse := strconv.ParseBool(raw)
		if errParse != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid failed, expected true or false"})
			return
		}
		failed = &value
	}

	snapshotParam := strings.TrimSpace(c.Query("snapshot"))
	var snapshotID string
	var boundaries map[string]int64

	filter := usagestats.RecordFilter{
		Provider: c.Query("provider"),
		Model:    c.Query("model"),
		Account:  c.Query("account"),
		Failed:   failed,
	}

	statsDir := usagestats.Dir(h.logDirectory())

	if snapshotParam == "new" {
		if offset != 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "snapshot=new requires offset=0"})
			return
		}
		snap, errSnap := usagestats.DefaultSnapshotStore.CreateSnapshot(statsDir, from, to, filter)
		if errSnap != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errSnap.Error()})
			return
		}
		snapshotID = snap.ID
		boundaries = snap.Boundaries
	} else if snapshotParam != "" {
		snap, errSnap := usagestats.DefaultSnapshotStore.GetSnapshot(snapshotParam, statsDir, from, to, filter)
		if errSnap != nil {
			if errors.Is(errSnap, usagestats.ErrSnapshotExpired) {
				c.JSON(http.StatusConflict, gin.H{"error": "snapshot expired or not found"})
				return
			}
			if errors.Is(errSnap, usagestats.ErrSnapshotCorrupted) {
				c.JSON(http.StatusConflict, gin.H{"error": "snapshot file corrupted or truncated"})
				return
			}
			if errors.Is(errSnap, usagestats.ErrSnapshotMismatch) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "snapshot parameters mismatch"})
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": errSnap.Error()})
			return
		}
		snapshotID = snap.ID
		boundaries = snap.Boundaries
	}

	page, err := usagestats.QueryRecordsWithOptions(statsDir, from, to, filter, offset, limit, boundaries)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	resp := gin.H{
		"from":        from.Format(time.RFC3339Nano),
		"to":          to.Format(time.RFC3339Nano),
		"records":     page.Records,
		"has_more":    page.HasMore,
		"next_offset": page.NextOff,
	}
	if snapshotID != "" {
		resp["snapshot_id"] = snapshotID
	}

	c.JSON(http.StatusOK, resp)
}

func parseNonNegativeInt(raw string, fallback int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, fmt.Errorf("value must be non-negative")
	}
	return value, nil
}
