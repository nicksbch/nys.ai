package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Sample is one reading of the host's CPU, memory and load. A value of -1
// means "not available" (e.g. CPU and memory on macOS, which has no /proc).
type Sample struct {
	Time  time.Time `json:"t"`
	CPU   float64   `json:"cpu"`   // % busy since the previous sample
	Mem   float64   `json:"mem"`   // % of memory in use (excluding cache)
	Load1 float64   `json:"load1"` // 1-minute load average
}

// Disk is the usage of the filesystem holding Path, counted the way df does:
// space reserved for root is neither used nor free.
type Disk struct {
	Path  string  `json:"path"`
	Total uint64  `json:"total"`
	Used  uint64  `json:"used"`
	Free  uint64  `json:"free"`
	Pct   float64 `json:"pct"`
	Err   string  `json:"error,omitempty"`
}

func diskUsage(path string) Disk {
	d := Disk{Path: path}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		d.Err = err.Error()
		return d
	}
	bs := uint64(st.Bsize)
	d.Total = st.Blocks * bs
	d.Free = st.Bavail * bs
	d.Used = (st.Blocks - st.Bfree) * bs
	if d.Used+d.Free > 0 {
		d.Pct = 100 * float64(d.Used) / float64(d.Used+d.Free)
	}
	return d
}

type cpuTimes struct{ idle, total uint64 }

// readCPU returns the cumulative CPU times from the first line of /proc/stat.
func readCPU() (cpuTimes, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	defer f.Close()
	line, _ := bufio.NewReader(f).ReadString('\n')
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}, false
	}
	// user nice system idle iowait irq softirq steal; guest time is already
	// included in user, so later columns are skipped.
	var t cpuTimes
	for i, s := range fields[1:min(len(fields), 9)] {
		v, _ := strconv.ParseUint(s, 10, 64)
		t.total += v
		if i == 3 || i == 4 { // idle, iowait
			t.idle += v
		}
	}
	return t, true
}

func cpuPercent(prev, cur cpuTimes) float64 {
	dt := cur.total - prev.total
	if cur.total <= prev.total {
		return -1
	}
	return 100 * float64(dt-(cur.idle-prev.idle)) / float64(dt)
}

func readMem() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return -1
	}
	defer f.Close()
	var total, avail float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(fields[1], 64)
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	if total == 0 {
		return -1
	}
	return 100 * (total - avail) / total
}

func readLoad() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return -1
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return -1
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return -1
	}
	return v
}
