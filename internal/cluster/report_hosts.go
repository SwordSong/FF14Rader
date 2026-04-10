package cluster

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultScanDir             = "./downloads/fflogsx"
	defaultClusterControlPort  = "22027"
	defaultControlEndpointPath = "/api/cluster/reports/execute"
)

var reportCodeRegexp = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`)

// ReportHostRegistry 维护 reportCode -> host 的映射，并带有按负载分配能力。
type ReportHostRegistry struct {
	mu                  sync.RWMutex
	reportHost          map[string]string
	hostLoad            map[string]int
	hostSeenAt          map[string]time.Time
	hostControlEndpoint map[string]string
}

var globalReportHostRegistry = NewReportHostRegistry()

// NewReportHostRegistry 创建一个新的 ReportHostRegistry 实例。
func NewReportHostRegistry() *ReportHostRegistry {
	return &ReportHostRegistry{
		reportHost:          make(map[string]string),
		hostLoad:            make(map[string]int),
		hostSeenAt:          make(map[string]time.Time),
		hostControlEndpoint: make(map[string]string),
	}
}

// GlobalReportHostRegistry 返回全局单例的 ReportHostRegistry 实例。
func GlobalReportHostRegistry() *ReportHostRegistry {
	return globalReportHostRegistry
}

// LocalHost 尝试从环境变量或系统信息中获取本机可用的 host 标识，优先级：CLUSTER_LOCAL_HOST > NODE_HOST > HOST > os.Hostname() > "127.0.0.1"
func LocalHost() string {
	candidates := []string{
		strings.TrimSpace(os.Getenv("CLUSTER_LOCAL_HOST")),
		strings.TrimSpace(os.Getenv("NODE_HOST")),
		strings.TrimSpace(os.Getenv("HOST")),
	}
	for _, c := range candidates {
		if n := NormalizeHost(c); n != "" {
			return n
		}
	}

	if host, err := os.Hostname(); err == nil {
		if n := NormalizeHost(host); n != "" {
			return n
		}
	}
	return "127.0.0.1"
}

func ReportsScanDir() string {
	if raw := strings.TrimSpace(os.Getenv("REPORTS_SCAN_DIR")); raw != "" {
		return raw
	}
	return defaultScanDir
}

func ClusterControlPort() string {
	if raw := strings.TrimSpace(os.Getenv("CLUSTER_CONTROL_PORT")); raw != "" {
		return strings.TrimPrefix(raw, ":")
	}
	if raw := strings.TrimSpace(os.Getenv("MONITOR_PORT")); raw != "" {
		return strings.TrimPrefix(raw, ":")
	}
	return defaultClusterControlPort
}

func NormalizeControlEndpoint(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}

	if !strings.Contains(v, "://") {
		v = "http://" + v
	}

	u, err := url.Parse(v)
	if err != nil {
		return ""
	}
	if strings.TrimSpace(u.Host) == "" {
		return ""
	}

	u.Scheme = strings.ToLower(strings.TrimSpace(u.Scheme))
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	u.Fragment = ""
	u.RawQuery = ""
	if strings.TrimSpace(u.Path) == "" || u.Path == "/" {
		u.Path = defaultControlEndpointPath
	}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	u.RawPath = ""
	return strings.TrimSpace(u.String())
}

func LocalControlEndpoint() string {
	if endpoint := NormalizeControlEndpoint(os.Getenv("CLUSTER_CONTROL_ENDPOINT")); endpoint != "" {
		return endpoint
	}

	host := NormalizeHost(os.Getenv("CLUSTER_CONTROL_HOST"))
	if host == "" {
		host = LocalHost()
	}

	u := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, ClusterControlPort()),
		Path:   defaultControlEndpointPath,
	}
	return u.String()
}

func NormalizeReportCode(raw string) string {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if code == "" {
		return ""
	}
	if !reportCodeRegexp.MatchString(code) {
		return ""
	}
	return code
}

func NormalizeHost(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}

	if strings.Contains(v, "://") {
		u, err := url.Parse(v)
		if err == nil {
			h := strings.TrimSpace(u.Hostname())
			if h != "" {
				return strings.ToLower(h)
			}
		}
	}

	if strings.Contains(v, "/") {
		parts := strings.SplitN(v, "/", 2)
		v = parts[0]
	}

	if host, _, err := net.SplitHostPort(v); err == nil {
		v = host
	} else if strings.Count(v, ":") == 1 {
		parts := strings.SplitN(v, ":", 2)
		if parts[0] != "" {
			v = parts[0]
		}
	}

	return strings.ToLower(strings.Trim(strings.TrimSpace(v), "[]"))
}

func containsHost(hosts []string, host string) bool {
	for _, h := range hosts {
		if NormalizeHost(h) == host {
			return true
		}
	}
	return false
}

func dedupeHosts(hosts []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		n := NormalizeHost(h)
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

// CollectReportCodesFromDir 扫描目录，提取可用 reportCode 列表（去重、排序）。
func CollectReportCodesFromDir(dirPath string) ([]string, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, err
	}

	set := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name())
		if name == "" {
			continue
		}
		if entry.IsDir() {
			if code := NormalizeReportCode(name); code != "" {
				set[code] = struct{}{}
			}
			continue
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		if code := NormalizeReportCode(base); code != "" {
			set[code] = struct{}{}
		}
	}

	out := make([]string, 0, len(set))
	for code := range set {
		out = append(out, code)
	}
	sort.Strings(out)
	return out, nil
}

func (r *ReportHostRegistry) touchHostLocked(host string, now time.Time) {
	r.hostSeenAt[host] = now
	if _, ok := r.hostLoad[host]; !ok {
		r.hostLoad[host] = 0
	}
}

func (r *ReportHostRegistry) setControlEndpointLocked(host, endpoint string) {
	normalized := NormalizeControlEndpoint(endpoint)
	if normalized == "" {
		return
	}
	r.hostControlEndpoint[host] = normalized
}

// SeedHostReportsFromDir 扫描目录下的报告名并注册到 host。
func (r *ReportHostRegistry) SeedHostReportsFromDir(host, dirPath string) (int, error) {
	h := NormalizeHost(host)
	if h == "" {
		h = LocalHost()
	}

	reports, err := CollectReportCodesFromDir(dirPath)
	if err != nil {
		return 0, err
	}

	added, _ := r.RegisterHostReports(h, reports)
	return added, nil
}

func (r *ReportHostRegistry) RegisterHostReports(host string, reports []string) (added int, total int) {
	return r.RegisterHostReportsWithEndpoint(host, reports, "")
}

func (r *ReportHostRegistry) RegisterHostReportsWithEndpoint(host string, reports []string, controlEndpoint string) (added int, total int) {
	h := NormalizeHost(host)
	if h == "" {
		return 0, len(r.reportHost)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.setControlEndpointLocked(h, controlEndpoint)
	r.touchHostLocked(h, time.Now())

	for _, raw := range reports {
		code := NormalizeReportCode(raw)
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

// HeartbeatHost 仅刷新 host 存活时间，不改动 reports 映射。
func (r *ReportHostRegistry) HeartbeatHost(host string) bool {
	return r.HeartbeatHostWithEndpoint(host, "")
}

// HeartbeatHostWithEndpoint 刷新 host 存活时间；如果携带了 endpoint 则一并更新。
func (r *ReportHostRegistry) HeartbeatHostWithEndpoint(host, controlEndpoint string) bool {
	h := NormalizeHost(host)
	if h == "" {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.setControlEndpointLocked(h, controlEndpoint)
	r.touchHostLocked(h, time.Now())
	return true
}

// RestoreHostWithEndpoint 从持久化状态恢复 host 的 seenAt 与 endpoint。
func (r *ReportHostRegistry) RestoreHostWithEndpoint(host, controlEndpoint string, seenAt time.Time) bool {
	h := NormalizeHost(host)
	if h == "" {
		return false
	}
	if seenAt.IsZero() {
		seenAt = time.Now()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.setControlEndpointLocked(h, controlEndpoint)
	existingSeen, ok := r.hostSeenAt[h]
	if !ok || existingSeen.IsZero() || seenAt.After(existingSeen) {
		r.hostSeenAt[h] = seenAt
	}
	if _, ok := r.hostLoad[h]; !ok {
		r.hostLoad[h] = 0
	}
	return true
}

func (r *ReportHostRegistry) ResolveHostControlEndpoint(host string) (string, bool) {
	h := NormalizeHost(host)
	if h == "" {
		return "", false
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	endpoint, ok := r.hostControlEndpoint[h]
	if !ok || strings.TrimSpace(endpoint) == "" {
		return "", false
	}
	return endpoint, true
}

func (r *ReportHostRegistry) ResolveDispatchExecuteURL(host, fallbackPort string) string {
	h := NormalizeHost(host)
	if h == "" {
		return ""
	}

	if endpoint, ok := r.ResolveHostControlEndpoint(h); ok {
		return endpoint
	}

	port := strings.TrimSpace(strings.TrimPrefix(fallbackPort, ":"))
	if port == "" {
		port = ClusterControlPort()
	}
	u := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(h, port),
		Path:   defaultControlEndpointPath,
	}
	return u.String()
}

func (r *ReportHostRegistry) ResolveHost(reportCode string) (string, bool) {
	code := NormalizeReportCode(reportCode)
	if code == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	host, ok := r.reportHost[code]
	return host, ok
}

// RestoreReportHost 从持久化状态恢复 reportCode -> host 映射。
func (r *ReportHostRegistry) RestoreReportHost(reportCode, host string) bool {
	code := NormalizeReportCode(reportCode)
	h := NormalizeHost(host)
	if code == "" || h == "" {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.reportHost[code] = h
	if _, ok := r.hostLoad[h]; !ok {
		r.hostLoad[h] = 0
	}
	if _, ok := r.hostSeenAt[h]; !ok {
		r.hostSeenAt[h] = time.Now()
	}
	return true
}

// AssignHostForReport 若 report 已有映射优先复用，否则按当前 host 负载最小策略分配。
func (r *ReportHostRegistry) AssignHostForReport(reportCode string, fightCount int, candidateHosts []string) string {
	code := NormalizeReportCode(reportCode)
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

// EvictExpiredHosts 剔除超时未心跳的 host，并删除其 reportCode 映射。
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
		delete(r.hostControlEndpoint, host)
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

func (r *ReportHostRegistry) Snapshot() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.reportHost))
	for k, v := range r.reportHost {
		out[k] = v
	}
	return out
}

func (r *ReportHostRegistry) SnapshotLoads() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int, len(r.hostLoad))
	for k, v := range r.hostLoad {
		out[k] = v
	}
	return out
}

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

func (r *ReportHostRegistry) SnapshotControlEndpoints() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.hostControlEndpoint))
	for host, endpoint := range r.hostControlEndpoint {
		if strings.TrimSpace(endpoint) == "" {
			continue
		}
		out[host] = endpoint
	}
	return out
}
