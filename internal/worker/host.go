package worker

// hostStats is the machine the worker runs on, flattened into GET /api/health
// so a fleet manager can see how loaded a node is next to how many channels it
// runs. RAM is the limit that matters for this program (about 15 MB per Mbps
// of input), so it comes first. Every figure is best effort: what the platform
// cannot supply stays zero, and a controller shows it blank.
//
// Sizes are binary: MB is 2^20 bytes, GB is 2^30. Rates are over the last
// sample, one second apart.
type hostStats struct {
	CPUPercent    float64 `json:"cpu_percent"` // whole host, all cores
	RAMPercent    float64 `json:"ram_percent"`
	RAMUsedMB     int64   `json:"ram_used_mb"` // total minus available, as free(1) counts it
	RAMTotalMB    int64   `json:"ram_total_mb"`
	DiskUsedGB    float64 `json:"disk_used_gb"` // the filesystem that holds the spool
	DiskTotalGB   float64 `json:"disk_total_gb"`
	NetInMbps     float64 `json:"net_in_mbps"` // every interface but loopback
	NetOutMbps    float64 `json:"net_out_mbps"`
	LinkSpeedMbps int     `json:"link_speed_mbps"` // the fastest interface that is up; 0 when unknown
	RSSMB         int64   `json:"rss_mb"`          // this process
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
