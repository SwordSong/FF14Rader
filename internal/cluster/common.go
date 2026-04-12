package cluster

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	defaultScanDir = "./downloads/fflogsx"
)

var reportCodeRegexp = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`)

// LocalHost 返回本地主机信息。
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

// ReportsScanDir 返回报告列表扫描目录。
func ReportsScanDir() string {
	if raw := strings.TrimSpace(os.Getenv("REPORTS_SCAN_DIR")); raw != "" {
		return raw
	}
	return defaultScanDir
}

// NormalizeReportCode 解析报告代码。
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

// NormalizeHost 规范化主机字符串。
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

// CollectReportCodesFromDir 扫描目录并收集合法报告编号列表。
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
