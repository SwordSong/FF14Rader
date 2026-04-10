package cluster

import (
	"fmt"
	"sort"
	"time"

	"github.com/user/ff14rader/internal/db"
	"github.com/user/ff14rader/internal/models"
	"gorm.io/gorm/clause"
)

// PersistHostEndpoint 持久化 host -> control endpoint 映射与最近心跳时间。
func PersistHostEndpoint(host, controlEndpoint string, seenAt time.Time) error {
	normalizedHost := NormalizeHost(host)
	if normalizedHost == "" {
		return fmt.Errorf("invalid host")
	}
	if seenAt.IsZero() {
		seenAt = time.Now()
	}

	normalizedEndpoint := NormalizeControlEndpoint(controlEndpoint)
	row := models.ClusterHostEndpoint{
		Host:       normalizedHost,
		LastSeenAt: seenAt,
	}
	if normalizedEndpoint != "" {
		row.ControlEndpoint = normalizedEndpoint
	}

	updates := map[string]interface{}{
		"last_seen_at": seenAt,
		"updated_at":   seenAt,
	}
	if normalizedEndpoint != "" {
		updates["control_endpoint"] = normalizedEndpoint
	}

	return db.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "host"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(&row).Error
}

// PersistReportHostMappings 持久化 reportCode -> host 映射（按 report_code 唯一键 upsert）。
func PersistReportHostMappings(host string, reportCodes []string, assignedAt time.Time) (int, error) {
	normalizedHost := NormalizeHost(host)
	if normalizedHost == "" {
		return 0, fmt.Errorf("invalid host")
	}
	if assignedAt.IsZero() {
		assignedAt = time.Now()
	}

	seen := make(map[string]struct{}, len(reportCodes))
	rows := make([]models.ClusterReportHost, 0, len(reportCodes))
	for _, raw := range reportCodes {
		code := NormalizeReportCode(raw)
		if code == "" {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		rows = append(rows, models.ClusterReportHost{
			ReportCode:     code,
			Host:           normalizedHost,
			LastAssignedAt: assignedAt,
		})
	}
	if len(rows) == 0 {
		return 0, nil
	}

	err := db.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "report_code"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"host":             normalizedHost,
			"last_assigned_at": assignedAt,
			"updated_at":       assignedAt,
		}),
	}).Create(&rows).Error
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// RestorePersistedHostEndpoints 启动时从数据库恢复 host->endpoint 映射到内存 registry。
func RestorePersistedHostEndpoints(registry *ReportHostRegistry) (int, error) {
	if registry == nil {
		registry = GlobalReportHostRegistry()
	}

	ttl := ClusterHostTTL()
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	cutoff := time.Now().Add(-ttl)

	var rows []models.ClusterHostEndpoint
	if err := db.DB.Model(&models.ClusterHostEndpoint{}).
		Where("last_seen_at >= ?", cutoff).
		Order("last_seen_at DESC").
		Find(&rows).Error; err != nil {
		return 0, err
	}

	applied := 0
	for _, row := range rows {
		if registry.RestoreHostWithEndpoint(row.Host, row.ControlEndpoint, row.LastSeenAt) {
			applied++
		}
	}
	return applied, nil
}

// RestorePersistedReportHosts 启动时从数据库恢复 reportCode->host 映射到内存 registry。
func RestorePersistedReportHosts(registry *ReportHostRegistry) (int, error) {
	if registry == nil {
		registry = GlobalReportHostRegistry()
	}

	seenMap := registry.SnapshotSeenAt()
	if len(seenMap) == 0 {
		return 0, nil
	}

	activeHosts := make([]string, 0, len(seenMap))
	for host := range seenMap {
		n := NormalizeHost(host)
		if n == "" {
			continue
		}
		activeHosts = append(activeHosts, n)
	}
	if len(activeHosts) == 0 {
		return 0, nil
	}
	sort.Strings(activeHosts)

	var rows []models.ClusterReportHost
	if err := db.DB.Model(&models.ClusterReportHost{}).
		Where("host IN ?", activeHosts).
		Order("last_assigned_at DESC").
		Find(&rows).Error; err != nil {
		return 0, err
	}

	applied := 0
	for _, row := range rows {
		if registry.RestoreReportHost(row.ReportCode, row.Host) {
			applied++
		}
	}
	return applied, nil
}
