package sysinfo

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Stats holds a snapshot of host resource usage.
type Stats struct {
	CPUPercent  float64 `json:"cpu_percent"`
	MemUsedMB   uint64  `json:"mem_used_mb"`
	MemTotalMB  uint64  `json:"mem_total_mb"`
	MemPercent  float64 `json:"mem_percent"`
	DiskUsedGB  uint64  `json:"disk_used_gb"`
	DiskTotalGB uint64  `json:"disk_total_gb"`
	DiskPercent float64 `json:"disk_percent"`
}

// cached holds the latest sampled Stats pointer (atomically swapped).
var cached atomic.Pointer[Stats]

// Start launches a background goroutine that samples stats every 2 seconds.
// Call this once from main after the app context is created.
func Start(ctx context.Context) {
	go func() {
		// Prime the cache immediately with a blocking sample.
		if s, err := sample(); err == nil {
			cached.Store(s)
		}
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if s, err := sample(); err == nil {
					cached.Store(s)
				}
			}
		}
	}()
}

// Get returns the latest cached Stats. If the background sampler has not yet
// produced a value (e.g., called before Start), it falls back to a blocking
// sample (200 ms) so the first request always returns real data.
func Get() (*Stats, error) {
	if s := cached.Load(); s != nil {
		return s, nil
	}
	return sample()
}

// sample collects a full Stats snapshot. CPU measurement blocks ~200ms.
func sample() (*Stats, error) {
	cpu, err := cpuPercent()
	if err != nil {
		return nil, err
	}
	memUsed, memTotal, err := memInfo()
	if err != nil {
		return nil, err
	}
	diskUsed, diskTotal, err := diskInfo("/data")
	if err != nil {
		diskUsed, diskTotal, _ = diskInfo("/")
	}
	var memPercent, diskPercent float64
	if memTotal > 0 {
		memPercent = 100 * float64(memUsed) / float64(memTotal)
	}
	if diskTotal > 0 {
		diskPercent = 100 * float64(diskUsed) / float64(diskTotal)
	}
	return &Stats{
		CPUPercent:  cpu,
		MemUsedMB:   memUsed,
		MemTotalMB:  memTotal,
		MemPercent:  memPercent,
		DiskUsedGB:  diskUsed,
		DiskTotalGB: diskTotal,
		DiskPercent: diskPercent,
	}, nil
}

type cpuSample struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func readCPUSample() (cpuSample, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSample{}, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			break
		}
		parse := func(s string) uint64 { v, _ := strconv.ParseUint(s, 10, 64); return v }
		return cpuSample{
			user:    parse(fields[1]),
			nice:    parse(fields[2]),
			system:  parse(fields[3]),
			idle:    parse(fields[4]),
			iowait:  parse(fields[5]),
			irq:     parse(fields[6]),
			softirq: parse(fields[7]),
			steal:   func() uint64 { if len(fields) > 8 { return parse(fields[8]) }; return 0 }(),
		}, nil
	}
	return cpuSample{}, nil
}

func cpuPercent() (float64, error) {
	s1, err := readCPUSample()
	if err != nil {
		return 0, err
	}
	time.Sleep(200 * time.Millisecond)
	s2, err := readCPUSample()
	if err != nil {
		return 0, err
	}
	idle1 := s1.idle + s1.iowait
	idle2 := s2.idle + s2.iowait
	total1 := s1.user + s1.nice + s1.system + idle1 + s1.irq + s1.softirq + s1.steal
	total2 := s2.user + s2.nice + s2.system + idle2 + s2.irq + s2.softirq + s2.steal
	totalDiff := float64(total2 - total1)
	idleDiff := float64(idle2 - idle1)
	if totalDiff == 0 {
		return 0, nil
	}
	return 100 * (totalDiff - idleDiff) / totalDiff, nil
}

func memInfo() (usedMB, totalMB uint64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	var total, available uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = val
		case "MemAvailable:":
			available = val
		}
	}
	totalMB = total / 1024
	usedMB = (total - available) / 1024
	return usedMB, totalMB, nil
}

func diskInfo(path string) (usedGB, totalGB uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	totalGB = total / (1024 * 1024 * 1024)
	usedGB = (total - free) / (1024 * 1024 * 1024)
	return usedGB, totalGB, nil
}
