package sysinfo

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Stats holds a snapshot of host resource usage.
type Stats struct {
	MemUsedMB   uint64  `json:"mem_used_mb"`
	MemTotalMB  uint64  `json:"mem_total_mb"`
	MemPercent  float64 `json:"mem_percent"`
	DiskUsedGB  uint64  `json:"disk_used_gb"`
	DiskTotalGB uint64  `json:"disk_total_gb"`
	DiskPercent float64 `json:"disk_percent"`
}

var (
	cacheMu  sync.Mutex
	cached   *Stats
	cachedAt time.Time
)

// Get returns a cached Stats snapshot, refreshing at most once per minute.
func Get() (*Stats, error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cached != nil && time.Since(cachedAt) < time.Minute {
		return cached, nil
	}
	s, err := sample()
	if err != nil {
		return nil, err
	}
	cached = s
	cachedAt = time.Now()
	return s, nil
}

// sample collects a Stats snapshot.
func sample() (*Stats, error) {
	memTotal, memAvail, err := readMemInfo()
	if err != nil {
		return nil, err
	}
	memUsed := memTotal - memAvail
	var memPct float64
	if memTotal > 0 {
		memPct = float64(memUsed) / float64(memTotal) * 100
	}

	var diskUsedGB, diskTotalGB uint64
	var diskPct float64
	for _, path := range []string{"/data", "/"} {
		used, total, err := diskUsage(path)
		if err == nil {
			diskUsedGB = used / (1024 * 1024 * 1024)
			diskTotalGB = total / (1024 * 1024 * 1024)
			if total > 0 {
				diskPct = float64(used) / float64(total) * 100
			}
			break
		}
	}

	return &Stats{
		MemUsedMB:   memUsed / (1024 * 1024),
		MemTotalMB:  memTotal / (1024 * 1024),
		MemPercent:  memPct,
		DiskUsedGB:  diskUsedGB,
		DiskTotalGB: diskTotalGB,
		DiskPercent: diskPct,
	}, nil
}

// readMemInfo reads MemTotal and MemAvailable from /proc/meminfo (bytes).
func readMemInfo() (total, available uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close() //nolint:errcheck // read-only file

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total, err = parseMemLine(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			available, err = parseMemLine(line)
		}
		if err != nil {
			return 0, 0, err
		}
		if total > 0 && available > 0 {
			return total, available, nil
		}
	}
	return 0, 0, errors.New("memTotal/memAvailable not found in /proc/meminfo")
}

// parseMemLine parses a /proc/meminfo line like "MemTotal:   16384 kB" and
// returns the value in bytes.
func parseMemLine(line string) (uint64, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected /proc/meminfo line: %q", line)
	}
	kb, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse /proc/meminfo value: %w", err)
	}
	return kb * 1024, nil
}

// diskUsage returns used and total bytes for the filesystem at path.
func diskUsage(path string) (used, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	total = st.Blocks * uint64(st.Bsize)
	avail := st.Bavail * uint64(st.Bsize)
	used = total - avail
	return used, total, nil
}
