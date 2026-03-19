package metrics

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Snapshot struct {
	CPU     CPUStats     `json:"cpu"`
	Memory  MemStats     `json:"memory"`
	Disk    DiskStats    `json:"disk"`
	Load    LoadStats    `json:"load"`
	Uptime  float64      `json:"uptime_seconds"`
	Sampled time.Time    `json:"sampled_at"`
}

type CPUStats struct {
	UsagePercent float64 `json:"usage_percent"`
	Cores        int     `json:"cores"`
}

type MemStats struct {
	TotalMB     uint64  `json:"total_mb"`
	UsedMB      uint64  `json:"used_mb"`
	FreeMB      uint64  `json:"free_mb"`
	UsedPercent float64 `json:"used_percent"`
}

type DiskStats struct {
	TotalGB     uint64  `json:"total_gb"`
	UsedGB      uint64  `json:"used_gb"`
	FreeGB      uint64  `json:"free_gb"`
	UsedPercent float64 `json:"used_percent"`
}

type LoadStats struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

// Collect lê todas as métricas de uma vez.
// CPU usa dois samples com 200ms de intervalo para calcular % real.
func Collect() (*Snapshot, error) {
	cpu, err := collectCPU()
	if err != nil {
		return nil, fmt.Errorf("cpu: %w", err)
	}
	mem, err := collectMem()
	if err != nil {
		return nil, fmt.Errorf("mem: %w", err)
	}
	disk, err := collectDisk("/")
	if err != nil {
		return nil, fmt.Errorf("disk: %w", err)
	}
	load, err := collectLoad()
	if err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}
	uptime, _ := collectUptime()

	return &Snapshot{
		CPU:     cpu,
		Memory:  mem,
		Disk:    disk,
		Load:    load,
		Uptime:  uptime,
		Sampled: time.Now(),
	}, nil
}

// cpuSample representa um ponto de leitura de /proc/stat
type cpuSample struct {
	total, idle uint64
}

func readCPUSample() (cpuSample, int, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSample{}, 0, err
	}
	defer f.Close()

	var s cpuSample
	var cores int
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)[1:]
			var vals []uint64
			for _, v := range fields {
				n, _ := strconv.ParseUint(v, 10, 64)
				vals = append(vals, n)
			}
			// user nice system idle iowait irq softirq steal
			if len(vals) >= 4 {
				for _, v := range vals {
					s.total += v
				}
				s.idle = vals[3]
				if len(vals) > 4 {
					s.idle += vals[4] // iowait
				}
			}
		}
		if strings.HasPrefix(line, "cpu") && !strings.HasPrefix(line, "cpu ") {
			cores++
		}
	}
	return s, cores, nil
}

func collectCPU() (CPUStats, error) {
	s1, cores, err := readCPUSample()
	if err != nil {
		return CPUStats{}, err
	}
	time.Sleep(200 * time.Millisecond)
	s2, _, err := readCPUSample()
	if err != nil {
		return CPUStats{}, err
	}

	totalDelta := float64(s2.total - s1.total)
	idleDelta := float64(s2.idle - s1.idle)
	usage := 0.0
	if totalDelta > 0 {
		usage = (1.0 - idleDelta/totalDelta) * 100.0
	}

	return CPUStats{
		UsagePercent: roundTwo(usage),
		Cores:        cores,
	}, nil
}

func collectMem() (MemStats, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return MemStats{}, err
	}
	defer f.Close()

	vals := make(map[string]uint64)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			key := strings.TrimSuffix(fields[0], ":")
			val, _ := strconv.ParseUint(fields[1], 10, 64)
			vals[key] = val // em kB
		}
	}

	total := vals["MemTotal"] / 1024
	free := (vals["MemFree"] + vals["Buffers"] + vals["Cached"] + vals["SReclaimable"]) / 1024
	used := total - free
	pct := 0.0
	if total > 0 {
		pct = roundTwo(float64(used) / float64(total) * 100)
	}

	return MemStats{
		TotalMB:     total,
		UsedMB:      used,
		FreeMB:      free,
		UsedPercent: pct,
	}, nil
}

func collectDisk(path string) (DiskStats, error) {
	// usa syscall diretamente para evitar import de "golang.org/x/sys"
	var stat syscallStatfs
	if err := syscallStatfsCall(path, &stat); err != nil {
		return DiskStats{}, err
	}
	total := stat.Blocks * uint64(stat.Bsize) / (1024 * 1024 * 1024)
	free := stat.Bfree * uint64(stat.Bsize) / (1024 * 1024 * 1024)
	used := total - free
	pct := 0.0
	if total > 0 {
		pct = roundTwo(float64(used) / float64(total) * 100)
	}
	return DiskStats{
		TotalGB:     total,
		UsedGB:      used,
		FreeGB:      free,
		UsedPercent: pct,
	}, nil
}

func collectLoad() (LoadStats, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return LoadStats{}, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return LoadStats{}, fmt.Errorf("formato inesperado em /proc/loadavg")
	}
	l1, _ := strconv.ParseFloat(fields[0], 64)
	l5, _ := strconv.ParseFloat(fields[1], 64)
	l15, _ := strconv.ParseFloat(fields[2], 64)
	return LoadStats{Load1: l1, Load5: l5, Load15: l15}, nil
}

func collectUptime() (float64, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, nil
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v, nil
}

func roundTwo(f float64) float64 {
	return float64(int(f*100)) / 100
}
