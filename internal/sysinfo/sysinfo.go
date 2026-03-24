package sysinfo

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
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
	percents, err := cpu.Percent(200*time.Millisecond, false)
	if err != nil {
		return nil, err
	}
	var cpuPct float64
	if len(percents) > 0 {
		cpuPct = percents[0]
	}

	vm, err := mem.VirtualMemory()
	if err != nil {
		return nil, err
	}

	var diskUsedGB, diskTotalGB uint64
	var diskPct float64
	setDisk := func(du *disk.UsageStat) {
		diskUsedGB = du.Used / (1024 * 1024 * 1024)
		diskTotalGB = du.Total / (1024 * 1024 * 1024)
		diskPct = du.UsedPercent
	}
	if du, err := disk.Usage("/data"); err == nil {
		setDisk(du)
	} else if du, err := disk.Usage("/"); err == nil {
		setDisk(du)
	}

	return &Stats{
		CPUPercent:  cpuPct,
		MemUsedMB:   vm.Used / (1024 * 1024),
		MemTotalMB:  vm.Total / (1024 * 1024),
		MemPercent:  vm.UsedPercent,
		DiskUsedGB:  diskUsedGB,
		DiskTotalGB: diskTotalGB,
		DiskPercent: diskPct,
	}, nil
}
