// Package metrics exports the daemon's view of the fleet to Prometheus.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AdithyaSM31/graywatch/internal/collect"
	"github.com/AdithyaSM31/graywatch/internal/detect"
)

type Metrics struct {
	signal  *prometheus.GaugeVec
	z       *prometheus.GaugeVec
	suspect *prometheus.GaugeVec
	actions *prometheus.CounterVec

	prevSignal, prevZ, prevSuspect series
}

// series maps a joined label key to its label values.
type series map[string][]string

func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		signal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "graywatch_signal",
			Help: "Latest value of each signal for each host.",
		}, []string{"host", "signal"}),
		z: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "graywatch_robust_z",
			Help: "Robust z-score of each signal against the host's peers.",
		}, []string{"host", "signal"}),
		suspect: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "graywatch_host_suspect",
			Help: "1 if the host is suspect, otherwise 0.",
		}, []string{"host"}),
		actions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "graywatch_actions_total",
			Help: "Actions and alerts by kind.",
		}, []string{"kind"}),
	}
	reg.MustRegister(m.signal, m.z, m.suspect, m.actions)
	return m
}

// Record updates the gauges for one tick. A series with no value this tick is
// removed rather than left at its last value, because missing is not the same as
// unchanged: a drained host has no latency at all.
func (m *Metrics) Record(samples []collect.Sample, obs detect.Observation) {
	signals := series{}
	for _, s := range samples {
		for sig, v := range s.Values {
			m.signal.WithLabelValues(s.Host, string(sig)).Set(v)
			signals[s.Host+"/"+string(sig)] = []string{s.Host, string(sig)}
		}
	}
	zs, suspects := series{}, series{}
	for _, v := range obs.Verdicts {
		for sig, z := range v.Z {
			m.z.WithLabelValues(v.Host, string(sig)).Set(z)
			zs[v.Host+"/"+string(sig)] = []string{v.Host, string(sig)}
		}
		value := 0.0
		if v.State == detect.Suspect {
			value = 1
		}
		m.suspect.WithLabelValues(v.Host).Set(value)
		suspects[v.Host] = []string{v.Host}
	}
	sweep(m.signal, m.prevSignal, signals)
	sweep(m.z, m.prevZ, zs)
	sweep(m.suspect, m.prevSuspect, suspects)
	m.prevSignal, m.prevZ, m.prevSuspect = signals, zs, suspects
}

func sweep(vec *prometheus.GaugeVec, prev, cur series) {
	for key, labels := range prev {
		if _, ok := cur[key]; !ok {
			vec.DeleteLabelValues(labels...)
		}
	}
}

func (m *Metrics) CountAction(kind string) {
	m.actions.WithLabelValues(kind).Inc()
}
