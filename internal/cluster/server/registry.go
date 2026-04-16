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
// reportHost的设计理念是快速查询report在哪个host上下载解析的，如果内部的host是空字符串，表示该report没有被任何host注册，外部调用方选择本地处理。
// 如果是本机解析的report，并且events数量大于0则让本机再次处理，这是为了让一个report的文件在一个主机上面处理，避免同一个report在多个主机上重复下载解析。对于分布式环境，建议外部调用方在分配host时优先考虑已经注册了该report的host，这样可以利用已经下载好的文件进行解析，减少网络传输和重复下载的开销。
type ReportHostRegistry struct {
	mu         sync.RWMutex
	user       map[int]models.PlayerLite  // playerID -> PlayerLite
	reportHost map[string]reportHostEntry // reportCode -> {events,host}
	hostLoad   map[string]int             // host -> 当前负载（可自定义为正在处理的报告数量等）
	hostSeenAt map[string]time.Time       // host -> 最近心跳时间
}

type reportHostEntry struct {
	Events int    `json:"events"`
	Host   string `json:"host"`
}

var globalReportHostRegistry = NewReportHostRegistry()

// NewReportHostRegistry 创建报告主机注册表。
func NewReportHostRegistry() *ReportHostRegistry {
	return &ReportHostRegistry{
		user:       make(map[int]models.PlayerLite),
		reportHost: make(map[string]reportHostEntry),
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

// touchHostLocked 更新主机最近心跳时间，必须在持有锁的情况下调用。
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

	r.mu.Lock()
	defer r.mu.Unlock()
	if h != "" {
		r.touchHostLocked(h, time.Now())
	}

	for _, raw := range reports {
		code := cluster.NormalizeReportCode(raw)
		if code == "" {
			continue
		}
		existing, exists := r.reportHost[code]
		if exists && existing.Host == h {
			continue
		}
		existing.Host = h
		r.reportHost[code] = existing
		added++
	}

	return added, len(r.reportHost)
}

// RegisterReportWithEvents 注册单个报告映射并可携带事件数量。
func (r *ReportHostRegistry) RegisterReportWithEvents(reportCode, host string, events int) (added int, total int) {
	code := cluster.NormalizeReportCode(reportCode)
	if code == "" {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return 0, len(r.reportHost)
	}

	h := cluster.NormalizeHost(host)
	if events < 0 {
		events = 0
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if h != "" {
		r.touchHostLocked(h, time.Now())
	}

	existing, exists := r.reportHost[code]
	if exists {
		existingHost := cluster.NormalizeHost(existing.Host)
		// 多主机重复上报同一 report 且事件数未变化时，保持现有路由，避免主机抖动与重复上报噪音。
		if existingHost != "" && existing.Events == events {
			return 0, len(r.reportHost)
		}
	}

	newEntry := existing
	if h != "" {
		newEntry.Host = h
	} else {
		newEntry.Host = cluster.NormalizeHost(existing.Host)
	}
	newEntry.Events = events

	if exists && existing.Host == newEntry.Host && existing.Events == newEntry.Events {
		return 0, len(r.reportHost)
	}

	r.reportHost[code] = newEntry
	return 1, len(r.reportHost)
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
	entry, ok := r.reportHost[code]
	if !ok {
		return "", false
	}
	host := cluster.NormalizeHost(entry.Host)
	if host == "" {
		return "", false
	}
	return host, true
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

	if entry, ok := r.reportHost[code]; ok {
		mapped := cluster.NormalizeHost(entry.Host)
		if mapped != "" && (len(candidates) == 0 || containsHost(candidates, mapped)) {
			entry.Host = mapped
			entry.Events = fightCount
			r.reportHost[code] = entry
			r.hostLoad[mapped] += fightCount
			r.touchHostLocked(mapped, time.Now())
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
	r.reportHost[code] = reportHostEntry{Events: fightCount, Host: chosen}
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
	for code, entry := range r.reportHost {
		host := cluster.NormalizeHost(entry.Host)
		if _, ok := expiredSet[host]; ok {
			delete(r.reportHost, code)
			removedReports++
		}
	}

	sort.Strings(expiredHosts)
	return expiredHosts, removedReports
}

// Snapshot 返回报告到主机映射快照。
func (r *ReportHostRegistry) Snapshot() map[string]reportHostEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]reportHostEntry, len(r.reportHost))
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
