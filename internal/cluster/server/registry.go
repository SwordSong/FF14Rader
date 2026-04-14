package server

import (
	"sort"
	"strings"
	"sync"
	"time"

	cluster "github.com/user/ff14rader/internal/cluster"
	"github.com/user/ff14rader/internal/models"
)

// ReportHostRegistry 维护 reportCode 到 host 的映射及主机负载信息。
type ReportHostRegistry struct {
	mu         sync.RWMutex
	user       map[int]models.PlayerLite
	reportHost map[string]string
	hostLoad   map[string]int
	hostSeenAt map[string]time.Time
}

var globalReportHostRegistry = NewReportHostRegistry()

// NewReportHostRegistry 创建报告主机注册表。
func NewReportHostRegistry() *ReportHostRegistry {
	return &ReportHostRegistry{
		user:       make(map[int]models.PlayerLite),
		reportHost: make(map[string]string),
		hostLoad:   make(map[string]int),
		hostSeenAt: make(map[string]time.Time),
	}
}

// GlobalReportHostRegistry 返回全局报告主机注册表实例。
func GlobalReportHostRegistry() *ReportHostRegistry {
	return globalReportHostRegistry
}

func containsHost(hosts []string, host string) bool {
	for _, h := range hosts {
		if cluster.NormalizeHost(h) == host {
			return true
		}
	}
	return false
}

func dedupeHosts(hosts []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		n := cluster.NormalizeHost(h)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

func (r *ReportHostRegistry) touchHostLocked(host string, now time.Time) {
	r.hostSeenAt[host] = now
	if _, ok := r.hostLoad[host]; !ok {
		r.hostLoad[host] = 0
	}
}

// SeedHostReportsFromDir 扫描目录并将报告映射注册到指定主机。
func (r *ReportHostRegistry) SeedHostReportsFromDir(host, dirPath string) (int, error) {
	h := cluster.NormalizeHost(host)
	if h == "" {
		h = cluster.LocalHost()
	}

	reports, err := cluster.CollectReportCodesFromDir(dirPath)
	if err != nil {
		return 0, err
	}

	added, _ := r.RegisterHostReports(h, reports)
	return added, nil
}

// RegisterHostReports 注册主机与报告映射。
func (r *ReportHostRegistry) RegisterHostReports(host string, reports []string) (added int, total int) {
	h := cluster.NormalizeHost(host)
	if h == "" {
		return 0, len(r.reportHost)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.touchHostLocked(h, time.Now())

	for _, raw := range reports {
		code := cluster.NormalizeReportCode(raw)
		if code == "" {
			continue
		}
		if existing, ok := r.reportHost[code]; ok && existing == h {
			continue
		}
		r.reportHost[code] = h
		added++
	}

	return added, len(r.reportHost)
}

// RegisterUserServer 注册玩家与服务器映射。
func (r *ReportHostRegistry) RegisterUserServer(player models.PlayerLite) bool {
	user := strings.TrimSpace(player.Name)
	server := strings.TrimSpace(player.Server)
	if user == "" || server == "" {
		return false
	}
	r.mu.Lock()
	r.user[player.PlayerID] = player
	r.mu.Unlock()
	return true
}

// SnapshotUsers 返回玩家与服务器映射快照。
func (r *ReportHostRegistry) SnapshotUsers() map[int]models.PlayerLite {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[int]models.PlayerLite, len(r.user))
	for k, v := range r.user {
		out[k] = v
	}
	return out
}

// ClaimUser 领取一个玩家映射并立即删除，避免多客户端重复处理同一条记录。
func (r *ReportHostRegistry) ClaimUser(host string) (int, models.PlayerLite, bool) {
	if cluster.NormalizeHost(host) == "" {
		return 0, models.PlayerLite{}, false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.user) == 0 {
		return 0, models.PlayerLite{}, false
	}

	ids := make([]int, 0, len(r.user))
	for playerID := range r.user {
		ids = append(ids, playerID)
	}
	sort.Ints(ids)

	playerID := ids[0]
	info := r.user[playerID]
	delete(r.user, playerID)
	return playerID, info, true
}

// HeartbeatHost 更新主机心跳时间。
func (r *ReportHostRegistry) HeartbeatHost(host string) bool {
	h := cluster.NormalizeHost(host)
	if h == "" {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.touchHostLocked(h, time.Now())
	return true
}

// ResolveHost 解析报告对应主机。
func (r *ReportHostRegistry) ResolveHost(reportCode string) (string, bool) {
	code := cluster.NormalizeReportCode(reportCode)
	if code == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	host, ok := r.reportHost[code]
	return host, ok
}

// AssignHostForReport 为报告分配主机并更新负载统计。
func (r *ReportHostRegistry) AssignHostForReport(reportCode string, fightCount int, candidateHosts []string) string {
	code := cluster.NormalizeReportCode(reportCode)
	if code == "" {
		return ""
	}
	if fightCount <= 0 {
		fightCount = 1
	}

	candidates := dedupeHosts(candidateHosts)
	r.mu.Lock()
	defer r.mu.Unlock()

	if mapped, ok := r.reportHost[code]; ok {
		if len(candidates) == 0 || containsHost(candidates, mapped) {
			r.hostLoad[mapped] += fightCount
			r.hostSeenAt[mapped] = time.Now()
			return mapped
		}
	}

	if len(candidates) == 0 {
		return ""
	}

	sort.Slice(candidates, func(i, j int) bool {
		li := r.hostLoad[candidates[i]]
		lj := r.hostLoad[candidates[j]]
		if li == lj {
			return candidates[i] < candidates[j]
		}
		return li < lj
	})

	chosen := candidates[0]
	r.reportHost[code] = chosen
	r.hostLoad[chosen] += fightCount
	r.touchHostLocked(chosen, time.Now())
	return chosen
}

// EvictExpiredHosts 清理超时主机并返回被剔除主机列表。
func (r *ReportHostRegistry) EvictExpiredHosts(ttl time.Duration) ([]string, int) {
	if ttl <= 0 {
		return nil, 0
	}

	cutoff := time.Now().Add(-ttl)

	r.mu.Lock()
	defer r.mu.Unlock()

	expiredSet := make(map[string]struct{})
	expiredHosts := make([]string, 0)
	for host, seenAt := range r.hostSeenAt {
		if seenAt.IsZero() || seenAt.Before(cutoff) {
			expiredSet[host] = struct{}{}
			expiredHosts = append(expiredHosts, host)
		}
	}
	if len(expiredHosts) == 0 {
		return nil, 0
	}

	for host := range expiredSet {
		delete(r.hostSeenAt, host)
		delete(r.hostLoad, host)
	}

	removedReports := 0
	for code, host := range r.reportHost {
		if _, ok := expiredSet[host]; ok {
			delete(r.reportHost, code)
			removedReports++
		}
	}

	sort.Strings(expiredHosts)
	return expiredHosts, removedReports
}

// Snapshot 返回报告到主机映射快照。
func (r *ReportHostRegistry) Snapshot() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.reportHost))
	for k, v := range r.reportHost {
		out[k] = v
	}
	return out
}

// SnapshotLoads 返回主机负载快照。
func (r *ReportHostRegistry) SnapshotLoads() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int, len(r.hostLoad))
	for k, v := range r.hostLoad {
		out[k] = v
	}
	return out
}

// SnapshotSeenAt 返回主机最近心跳时间快照。
func (r *ReportHostRegistry) SnapshotSeenAt() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.hostSeenAt))
	for host, ts := range r.hostSeenAt {
		if ts.IsZero() {
			continue
		}
		out[host] = ts.UTC().Format(time.RFC3339)
	}
	return out
}
