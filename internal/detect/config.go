package detect

import "github.com/AdithyaSM31/graywatch/internal/collect"

// appCategory is for symptoms with no kernel signal to name a cause.
const appCategory = "app"

type Config struct {
	Z                float64
	Window, Need     int
	MinHosts         int
	MinMAD, MinDelta map[collect.Signal]float64
	Category         map[collect.Signal]string
	FleetLatencyMs   float64
	FleetErrRate     float64
}

func DefaultConfig() Config {
	return Config{
		Z:        3.5,
		Window:   5,
		Need:     3,
		MinHosts: 5,
		MinMAD: map[collect.Signal]float64{
			collect.SigLatP90:   5,
			collect.SigErrRate:  0.005,
			collect.SigCPUPSI:   0.01,
			collect.SigThrottle: 0.01,
			collect.SigIOPSI:    0.01,
			collect.SigMemPSI:   0.01,
			collect.SigRetrans:  0.002,
		},
		MinDelta: map[collect.Signal]float64{
			collect.SigLatP90:   50,
			collect.SigErrRate:  0.02,
			collect.SigCPUPSI:   0.10,
			collect.SigThrottle: 0.10,
			collect.SigIOPSI:    0.10,
			collect.SigMemPSI:   0.05,
			collect.SigRetrans:  0.01,
		},
		Category: map[collect.Signal]string{
			collect.SigLatP90:   appCategory,
			collect.SigErrRate:  appCategory,
			collect.SigCPUPSI:   "cpu",
			collect.SigThrottle: "cpu",
			collect.SigIOPSI:    "disk",
			collect.SigMemPSI:   "memory",
			collect.SigRetrans:  "network",
		},
		FleetLatencyMs: 300,
		FleetErrRate:   0.05,
	}
}
