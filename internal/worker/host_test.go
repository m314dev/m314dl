package worker

import (
	"encoding/json"
	"runtime"
	"testing"
	"time"
)

// The health reply carries the machine: a second sample has rates, and the
// sizes are there from the first.
func TestHealthReportsHost(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("host stats are read from /proc")
	}
	w := newWorkerServer("", 32, "", "test", 0, nil)
	var hm hostMeter
	hm.sample(time.Now(), "")
	time.Sleep(20 * time.Millisecond)
	h := hm.sample(time.Now(), "")
	if h.RAMTotalMB <= 0 || h.RAMUsedMB <= 0 || h.RAMUsedMB > h.RAMTotalMB {
		t.Fatalf("ram: %+v", h)
	}
	if h.DiskTotalGB <= 0 || h.DiskUsedGB > h.DiskTotalGB {
		t.Fatalf("disk: %+v", h)
	}
	if h.CPUPercent < 0 || h.CPUPercent > 100 || h.RSSMB <= 0 {
		t.Fatalf("cpu/rss: %+v", h)
	}
	w.host.Store(&h)
	var out map[string]any
	json.Unmarshal(workerReq(t, w, "GET", "/api/health", "", "").Body.Bytes(), &out)
	for _, k := range []string{"status", "channels", "max", "version", "cpu_percent", "ram_percent", "ram_used_mb",
		"ram_total_mb", "disk_used_gb", "disk_total_gb", "net_in_mbps", "net_out_mbps", "link_speed_mbps", "rss_mb"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("health lacks %q: %v", k, out)
		}
	}
	if out["ram_total_mb"].(float64) != float64(h.RAMTotalMB) {
		t.Fatalf("health ram_total_mb %v, sampled %d", out["ram_total_mb"], h.RAMTotalMB)
	}
}
