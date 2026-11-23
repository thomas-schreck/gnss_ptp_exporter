// Package promtest gathers metrics from a collector for tests.
package promtest

import (
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// Gather registers c in a fresh registry, gathers it and returns the
// values keyed as `name{label="value",...}` (labels sorted, no braces
// without labels).
func Gather(t testing.TB, c prometheus.Collector) map[string]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			var ls []string
			for _, l := range m.GetLabel() {
				ls = append(ls, l.GetName()+"=\""+l.GetValue()+"\"")
			}
			sort.Strings(ls)
			key := mf.GetName()
			if len(ls) > 0 {
				key += "{" + strings.Join(ls, ",") + "}"
			}
			v := m.GetGauge().GetValue()
			if m.GetCounter() != nil {
				v = m.GetCounter().GetValue()
			}
			out[key] = v
		}
	}
	return out
}

// Expect fails the test for every key in want whose value differs or is
// missing in got.
func Expect(t testing.TB, got, want map[string]float64) {
	t.Helper()
	for k, v := range want {
		g, ok := got[k]
		switch {
		case !ok:
			t.Errorf("missing metric %s (want %v)", k, v)
		case g != v:
			t.Errorf("%s = %v, want %v", k, g, v)
		}
	}
}

// Absent fails the test for every key present in got.
func Absent(t testing.TB, got map[string]float64, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if v, ok := got[k]; ok {
			t.Errorf("metric %s = %v present, want absent", k, v)
		}
	}
}
