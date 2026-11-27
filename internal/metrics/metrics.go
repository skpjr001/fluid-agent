// Package metrics is a tiny Prometheus-compatible registry.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Metrics is a tiny Prometheus-compatible registry (counters and gauges with
// labels), enough for this agent without pulling in client_golang.
type Metrics struct {
	mu     sync.Mutex
	series map[string]*series
	gauges map[string]func() float64
	help   map[string]string
	types  map[string]string
}

type series struct {
	name   string
	labels string
	v      atomic.Int64
}

func NewMetrics() *Metrics {
	return &Metrics{series: map[string]*series{}, gauges: map[string]func() float64{},
		help: map[string]string{}, types: map[string]string{}}
}

func (m *Metrics) describe(name, typ, help string) {
	m.types[name] = typ
	m.help[name] = help
}

// Inc adds 1 to the counter name with alternating label key/value pairs.
func (m *Metrics) Inc(name string, kv ...string) { m.Add(name, 1, kv...) }

func (m *Metrics) Add(name string, n int64, kv ...string) {
	if m == nil {
		return
	}
	labels := formatLabels(kv)
	key := name + labels
	m.mu.Lock()
	s := m.series[key]
	if s == nil {
		s = &series{name: name, labels: labels}
		m.series[key] = s
		if _, ok := m.types[name]; !ok {
			m.types[name] = "counter"
		}
	}
	m.mu.Unlock()
	s.v.Add(n)
}

// Get returns a counter's current value (used by tests and the status API).
func (m *Metrics) Get(name string, kv ...string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.series[name+formatLabels(kv)]; s != nil {
		return s.v.Load()
	}
	return 0
}

// Gauge registers a function sampled at scrape time.
func (m *Metrics) Gauge(name, help string, fn func() float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[name] = fn
	m.describe(name, "gauge", help)
}

func formatLabels(kv []string) string {
	if len(kv) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(kv[i+1])
		fmt.Fprintf(&b, `%s="%s"`, kv[i], v)
	}
	b.WriteByte('}')
	return b.String()
}

// WritePrometheus renders the text exposition format.
func (m *Metrics) WritePrometheus(w io.Writer) {
	m.mu.Lock()
	byName := map[string][]*series{}
	for _, s := range m.series {
		byName[s.name] = append(byName[s.name], s)
	}
	gauges := make(map[string]func() float64, len(m.gauges))
	for k, v := range m.gauges {
		gauges[k] = v
	}
	help := m.help
	types := m.types
	m.mu.Unlock()

	names := make([]string, 0, len(byName)+len(gauges))
	for n := range byName {
		names = append(names, n)
	}
	for n := range gauges {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if h := help[n]; h != "" {
			fmt.Fprintf(w, "# HELP %s %s\n", n, h)
		}
		fmt.Fprintf(w, "# TYPE %s %s\n", n, types[n])
		if fn, ok := gauges[n]; ok {
			fmt.Fprintf(w, "%s %g\n", n, fn())
			continue
		}
		ss := byName[n]
		sort.Slice(ss, func(i, j int) bool { return ss[i].labels < ss[j].labels })
		for _, s := range ss {
			fmt.Fprintf(w, "%s%s %d\n", n, s.labels, s.v.Load())
		}
	}
}

// Describe sets the Prometheus TYPE and HELP lines for the metric name.
func (m *Metrics) Describe(name, typ, help string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.describe(name, typ, help)
}
