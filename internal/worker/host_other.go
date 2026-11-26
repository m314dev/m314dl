//go:build !linux

package worker

import "time"

// hostMeter reads the machine. Only Linux has an implementation; elsewhere
// every figure stays zero.
type hostMeter struct{}

func (m *hostMeter) sample(time.Time, string) hostStats { return hostStats{} }
