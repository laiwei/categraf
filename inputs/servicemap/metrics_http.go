package servicemap

// metrics_http.go — 在 port 9099 上暴露 /metrics（Prometheus 文本格式）
//
// 策略：
//   - 每次 Gather() 完成后，非破坏性地扫描 slist（RLock + 遍历 list.List），
//     把所有样本序列化为 Prometheus text/plain 格式，写入 ins.promCache。
//   - /metrics 请求直接返回缓存字节，不会重复读取 eBPF 状态，不会影响计数器语义。

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"flashcat.cloud/categraf/types"
)

// ─────────────────────────────────────────────────────────────
// Instance 字段扩展（通过文件内 init 无法扩展 struct，
// 实际字段声明在 instance.go 的 Instance struct 中追加）
// ─────────────────────────────────────────────────────────────
// 见 instance.go: metricsMu sync.RWMutex / promCache []byte / promCacheAge time.Time

// cacheMetrics 在 Gather() 末尾调用，将 slist 中的样本格式化为 Prometheus
// text format 0.0.4 并写入 ins.promCache。
// 该函数对 slist 只做只读遍历，不会弹出任何元素。
func (ins *Instance) cacheMetrics(slist *types.SampleList) {
	// ── 1. 快照样本（持有 RLock，遍历 container/list.List）────────────
	slist.RLock()
	n := slist.L.Len()
	if n == 0 {
		slist.RUnlock()
		return
	}
	samples := make([]*types.Sample, 0, n)
	for e := slist.L.Front(); e != nil; e = e.Next() {
		if s, ok := e.Value.(*types.Sample); ok {
			samples = append(samples, s)
		}
	}
	slist.RUnlock()

	// ── 2. 按 metric 名称分组 ──────────────────────────────────────────
	type idx struct{ pos int }
	grouped := make(map[string][]int, len(samples))
	for i, s := range samples {
		grouped[s.Metric] = append(grouped[s.Metric], i)
	}

	names := make([]string, 0, len(grouped))
	for name := range grouped {
		names = append(names, name)
	}
	sort.Strings(names)

	// ── 3. 序列化为 Prometheus text format ────────────────────────────
	now := time.Now().UnixMilli()
	var buf bytes.Buffer

	for _, name := range names {
		typeName := promType(name)
		fmt.Fprintf(&buf, "# HELP %s servicemap network observability metric\n", name)
		fmt.Fprintf(&buf, "# TYPE %s %s\n", name, typeName)

		for _, i := range grouped[name] {
			s := samples[i]
			val := sampleToFloat64(s.Value)
			ts := now
			if !s.Timestamp.IsZero() {
				ts = s.Timestamp.UnixMilli()
			}
			fmt.Fprintf(&buf, "%s{%s} %s %d\n",
				name,
				formatLabelSet(s.Labels),
				formatFloat(val),
				ts,
			)
		}
	}

	// ── 4. 更新缓存 ────────────────────────────────────────────────────
	ins.metricsMu.Lock()
	ins.promCache = buf.Bytes()
	ins.promCacheAge = time.Now()
	ins.metricsMu.Unlock()
}

// handleMetrics 是 /metrics 的 HTTP 处理函数。
// 直接返回最近一次 Gather() 生成的 Prometheus 文本缓存。
func (ins *Instance) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ins.metricsMu.RLock()
	cache := ins.promCache
	age := ins.promCacheAge
	ins.metricsMu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if len(cache) == 0 {
		// 还没有任何 Gather() 完成过
		w.Header().Set("X-Metrics-Age", "not-yet-collected")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "# Metrics not yet collected. Waiting for first Gather() cycle.")
		return
	}

	ageMs := time.Since(age).Milliseconds()
	w.Header().Set("X-Metrics-Age-Ms", strconv.FormatInt(ageMs, 10))
	_, _ = w.Write(cache)
}

// ─────────────────────────────────────────────────────────────
// 辅助函数
// ─────────────────────────────────────────────────────────────

// promType 根据指标名称后缀推断 Prometheus 类型字符串。
func promType(name string) string {
	switch {
	case strings.HasSuffix(name, "_total"):
		return "counter"
	case strings.HasSuffix(name, "_seconds_count"),
		strings.HasSuffix(name, "_seconds_sum"),
		strings.HasSuffix(name, "_bucket"):
		return "untyped"
	case strings.HasSuffix(name, "_seconds"),
		strings.HasSuffix(name, "_ratio"),
		strings.HasSuffix(name, "_celsius"),
		strings.HasSuffix(name, "_ratio"):
		return "gauge"
	default:
		// 包含 "active", "connections", "ports", "containers" 等词
		for _, kw := range []string{"active", "connections", "ports", "containers", "count", "up", "info"} {
			if strings.Contains(name, kw) {
				return "gauge"
			}
		}
		return "untyped"
	}
}

// sampleToFloat64 将 Sample.Value（interface{}）安全转换为 float64。
func sampleToFloat64(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case uint:
		return float64(x)
	case uint32:
		return float64(x)
	case uint64:
		return float64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	default:
		return math.NaN()
	}
}

// formatFloat 格式化浮点数为 Prometheus 文本格式认可的字符串。
func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	default:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
}

// formatLabelSet 将 map[string]string 格式化为 Prometheus 标签集字符串，
// 例如: key1="val1",key2="val2"
// 键名按字典序排序保证输出确定性。
func formatLabelSet(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(k)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(labels[k]))
		sb.WriteByte('"')
	}
	return sb.String()
}

// escapeLabelValue 对标签值中的特殊字符（\、"、\n）做转义。
func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// ─────────────────────────────────────────────────────────────
// 注意
// ─────────────────────────────────────────────────────────────
// metricsMu / promCache / promCacheAge 三个字段声明在 instance.go
// 的 Instance struct 中（Go 零值即可用，无需显式初始化）。
