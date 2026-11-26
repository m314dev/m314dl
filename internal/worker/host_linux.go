//go:build linux

package worker

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// hostMeter reads the machine from /proc and /sys. CPU and network are rates,
// so it keeps the counters of the previous sample and reports the difference;
// the first sample has nothing to compare against and reports zero for both.
type hostMeter struct {
	at                time.Time
	cpuBusy, cpuTotal uint64
	rx, tx            uint64
}

func (m *hostMeter) sample(now time.Time, spool string) hostStats {
	var s hostStats
	busy, total := cpuTicks()
	rx, tx := netBytes()
	if !m.at.IsZero() {
		if dt := total - m.cpuTotal; dt > 0 {
			s.CPUPercent = round1(100 * float64(busy-m.cpuBusy) / float64(dt))
		}
		if wall := now.Sub(m.at).Seconds(); wall > 0 {
			s.NetInMbps = round1(float64(rx-m.rx) * 8 / wall / 1e6)
			s.NetOutMbps = round1(float64(tx-m.tx) * 8 / wall / 1e6)
		}
	}
	m.at, m.cpuBusy, m.cpuTotal, m.rx, m.tx = now, busy, total, rx, tx

	if total, avail, ok := memInfo(); ok {
		s.RAMTotalMB = int64(total >> 10)
		s.RAMUsedMB = int64((total - avail) >> 10)
		s.RAMPercent = round1(100 * float64(total-avail) / float64(total))
	}
	if spool == "" {
		spool = os.TempDir()
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(spool, &fs); err == nil && fs.Blocks > 0 {
		bs := uint64(fs.Bsize)
		s.DiskTotalGB = round1(float64(fs.Blocks*bs) / (1 << 30))
		s.DiskUsedGB = round1(float64((fs.Blocks-fs.Bfree)*bs) / (1 << 30))
	}
	s.LinkSpeedMbps = linkSpeed()
	if f, err := os.ReadFile("/proc/self/statm"); err == nil {
		if fields := strings.Fields(string(f)); len(fields) > 1 {
			pages, _ := strconv.ParseInt(fields[1], 10, 64)
			s.RSSMB = pages * int64(os.Getpagesize()) >> 20
		}
	}
	return s
}

// cpuTicks reads the first line of /proc/stat: the jiffies every CPU spent in
// each state since boot. Busy is everything but idle and iowait.
func cpuTicks() (busy, total uint64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	line, _ := bufio.NewReader(f).ReadString('\n')
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}
	var idle uint64
	for i, v := range fields[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	return total - idle, total
}

// memInfo returns MemTotal and MemAvailable from /proc/meminfo, in kB.
func memInfo() (total, avail uint64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		n, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = n
		case "MemAvailable:":
			avail = n
		}
	}
	return total, avail, total > 0
}

// netBytes sums the receive and transmit byte counters of every interface in
// /proc/net/dev except loopback.
func netBytes() (rx, tx uint64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(fields[0], 10, 64)
		t, _ := strconv.ParseUint(fields[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx
}

// linkSpeed is the speed in Mbit/s of the fastest interface that is up, from
// /sys/class/net. Virtual interfaces report nothing or -1 and are skipped.
func linkSpeed() int {
	best := 0
	dirs, _ := filepath.Glob("/sys/class/net/*")
	for _, d := range dirs {
		if filepath.Base(d) == "lo" {
			continue
		}
		state, _ := os.ReadFile(filepath.Join(d, "operstate"))
		if strings.TrimSpace(string(state)) != "up" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(d, "speed"))
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && n > best {
			best = n
		}
	}
	return best
}
