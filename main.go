package main

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	gopsnet "github.com/shirou/gopsutil/v3/net"
	gopsprocess "github.com/shirou/gopsutil/v3/process"
)

//go:embed web
var webFS embed.FS

type Stats struct {
	Hostname      string           `json:"hostname"`
	TailnetDomain string           `json:"tailnet_domain"`
	UptimeSeconds uint64           `json:"uptime_seconds"`
	BootTime      uint64           `json:"boot_time"`
	OS            string           `json:"os"`
	Kernel        string           `json:"kernel"`
	Platform      string           `json:"platform"`
	Procs         uint64           `json:"procs"`
	CPU           CPUStats         `json:"cpu"`
	Memory        MemStats         `json:"memory"`
	Swap          SwapStats        `json:"swap"`
	Disks         []DiskStats      `json:"disks"`
	DiskIO        DiskIOStats      `json:"disk_io"`
	Network       NetworkStats     `json:"network"`
	LoadAvg       LoadAvgStats     `json:"load_avg"`
	Containers    []ContainerStats `json:"containers"`
	Temps         []TempStat       `json:"temps"`
	TopProcs      []ProcStat       `json:"top_procs"`
	Conns         []ConnStat       `json:"conns"`
	Timestamp     time.Time        `json:"timestamp"`
}

type CPUStats struct {
	Model         string  `json:"model"`
	PhysicalCores int     `json:"physical_cores"`
	LogicalCores  int     `json:"logical_cores"`
	UsagePercent  float64 `json:"usage_percent"`
}

type MemStats struct {
	TotalGB     float64 `json:"total_gb"`
	UsedGB      float64 `json:"used_gb"`
	AvailableGB float64 `json:"available_gb"`
	Percent     float64 `json:"percent"`
}

type SwapStats struct {
	TotalGB float64 `json:"total_gb"`
	UsedGB  float64 `json:"used_gb"`
	Percent float64 `json:"percent"`
}

type DiskStats struct {
	Path    string  `json:"path"`
	Device  string  `json:"device"`
	FSType  string  `json:"fstype"`
	TotalGB float64 `json:"total_gb"`
	UsedGB  float64 `json:"used_gb"`
	FreeGB  float64 `json:"free_gb"`
	Percent float64 `json:"percent"`
}

type DiskIOStats struct {
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
}

type NetIface struct {
	Name      string `json:"name"`
	BytesSent uint64 `json:"bytes_sent"`
	BytesRecv uint64 `json:"bytes_recv"`
}

type NetworkStats struct {
	TailscaleIP string     `json:"tailscale_ip"`
	Interfaces  []NetIface `json:"interfaces"`
}

type LoadAvgStats struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

type ContainerStats struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	State  string `json:"state"`
	Status string `json:"status"`
}

type TempStat struct {
	Label    string  `json:"label"`
	Temp     float64 `json:"temp"`
	High     float64 `json:"high"`
	Critical float64 `json:"critical"`
}

type ProcStat struct {
	PID    int32   `json:"pid"`
	Name   string  `json:"name"`
	CPU    float64 `json:"cpu"`
	MemMB  float64 `json:"mem_mb"`
	MemPct float32 `json:"mem_pct"`
	State  string  `json:"state"`
}

type ConnStat struct {
	Proto      string `json:"proto"`
	LocalIP    string `json:"local_ip"`
	LocalPort  uint32 `json:"local_port"`
	RemoteIP   string `json:"remote_ip"`
	RemotePort uint32 `json:"remote_port"`
	Status     string `json:"status"`
	PID        int32  `json:"pid"`
	Process    string `json:"process"`
}

var (
	cachedProcs []ProcStat
	cachedConns []ConnStat
	cacheMu     sync.RWMutex
)

var skipFsTypes = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "devfs": true, "squashfs": true,
	"overlay": true, "aufs": true, "proc": true, "sysfs": true,
	"cgroup": true, "cgroup2": true, "pstore": true, "efivarfs": true,
	"bpf": true, "tracefs": true, "debugfs": true, "securityfs": true,
	"configfs": true, "ramfs": true, "hugetlbfs": true, "mqueue": true,
	"nsfs": true, "rpc_pipefs": true, "fusectl": true,
}

func bytesToGB(b uint64) float64 { return float64(b) / (1 << 30) }

func collectTemps() []TempStat {
	temps, err := host.SensorsTemperatures()
	if err != nil {
		return nil
	}
	var result []TempStat
	for _, t := range temps {
		name := strings.ToLower(t.SensorKey)
		if !strings.Contains(name, "core") && !strings.Contains(name, "cpu") &&
			!strings.Contains(name, "tctl") && !strings.Contains(name, "tdie") &&
			!strings.Contains(name, "package") {
			continue
		}
		result = append(result, TempStat{
			Label:    t.SensorKey,
			Temp:     t.Temperature,
			High:     t.High,
			Critical: t.Critical,
		})
	}
	return result
}

func gatherProcs() []ProcStat {
	procs, err := gopsprocess.Processes()
	if err != nil {
		return nil
	}
	for _, p := range procs {
		p.CPUPercent()
	}
	time.Sleep(200 * time.Millisecond)
	var result []ProcStat
	for _, p := range procs {
		cpuPct, _ := p.CPUPercent()
		memInfo, err := p.MemoryInfo()
		if err != nil {
			continue
		}
		memPct, _ := p.MemoryPercent()
		name, _ := p.Name()
		status, _ := p.Status()
		state := ""
		if len(status) > 0 {
			state = status[0]
		}
		result = append(result, ProcStat{
			PID:    p.Pid,
			Name:   name,
			CPU:    cpuPct,
			MemMB:  float64(memInfo.RSS) / (1 << 20),
			MemPct: memPct,
			State:  state,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return (result[i].CPU + float64(result[i].MemPct)) > (result[j].CPU + float64(result[j].MemPct))
	})
	if len(result) > 15 {
		result = result[:15]
	}
	return result
}

func gatherConns() []ConnStat {
	conns, err := gopsnet.Connections("inet")
	if err != nil {
		return nil
	}
	pidNames := map[int32]string{}
	if procs, err := gopsprocess.Processes(); err == nil {
		for _, p := range procs {
			if name, err := p.Name(); err == nil {
				pidNames[p.Pid] = name
			}
		}
	}
	var result []ConnStat
	for _, c := range conns {
		if c.Status != "LISTEN" && c.Status != "ESTABLISHED" {
			continue
		}
		proto := "tcp"
		if c.Type == 2 {
			proto = "udp"
		}
		if c.Family == 10 {
			proto += "6"
		}
		result = append(result, ConnStat{
			Proto:      proto,
			LocalIP:    c.Laddr.IP,
			LocalPort:  c.Laddr.Port,
			RemoteIP:   c.Raddr.IP,
			RemotePort: c.Raddr.Port,
			Status:     c.Status,
			PID:        c.Pid,
			Process:    pidNames[c.Pid],
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Status != result[j].Status {
			return result[i].Status == "LISTEN"
		}
		return result[i].LocalPort < result[j].LocalPort
	})
	if len(result) > 150 {
		result = result[:150]
	}
	return result
}

func startBackgroundCollectors() {
	go func() {
		for {
			procs := gatherProcs()
			conns := gatherConns()
			cacheMu.Lock()
			cachedProcs = procs
			cachedConns = conns
			cacheMu.Unlock()
			time.Sleep(10 * time.Second)
		}
	}()
}

func detectTailscaleIP() string {
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && ip.To4() != nil && cgnat.Contains(ip) {
				return ip.String()
			}
		}
	}
	return ""
}

func collectStats() Stats {
	tailnetDomain := os.Getenv("TAILNET_DOMAIN")
	s := Stats{Timestamp: time.Now(), TailnetDomain: tailnetDomain}

	if info, err := host.Info(); err == nil {
		s.Hostname = info.Hostname
		s.UptimeSeconds = info.Uptime
		s.BootTime = info.BootTime
		s.OS = info.Platform + " " + info.PlatformVersion
		s.Kernel = info.KernelVersion
		s.Platform = info.OS
		s.Procs = info.Procs
	}

	if pct, err := cpu.Percent(500*time.Millisecond, false); err == nil && len(pct) > 0 {
		s.CPU.UsagePercent = pct[0]
	}
	if infos, err := cpu.Info(); err == nil && len(infos) > 0 {
		s.CPU.Model = infos[0].ModelName
		s.CPU.PhysicalCores = int(infos[0].Cores)
	}
	if lc, err := cpu.Counts(true); err == nil {
		s.CPU.LogicalCores = lc
	}

	if v, err := mem.VirtualMemory(); err == nil {
		s.Memory = MemStats{
			TotalGB:     bytesToGB(v.Total),
			UsedGB:      bytesToGB(v.Used),
			AvailableGB: bytesToGB(v.Available),
			Percent:     v.UsedPercent,
		}
	}
	if sw, err := mem.SwapMemory(); err == nil {
		s.Swap = SwapStats{
			TotalGB: bytesToGB(sw.Total),
			UsedGB:  bytesToGB(sw.Used),
			Percent: sw.UsedPercent,
		}
	}

	diskRoot := os.Getenv("DISK_ROOT") // e.g. /host_root when running in Docker
	if parts, err := disk.Partitions(false); err == nil {
		seen := map[string]bool{}
		for _, p := range parts {
			if skipFsTypes[p.Fstype] || seen[p.Mountpoint] {
				continue
			}
			seen[p.Mountpoint] = true
			mountPath := p.Mountpoint
			if diskRoot != "" {
				mountPath = diskRoot + p.Mountpoint
			}
			if u, err := disk.Usage(mountPath); err == nil && u.Total > 0 {
				s.Disks = append(s.Disks, DiskStats{
					Path:    p.Mountpoint,
					Device:  p.Device,
					FSType:  p.Fstype,
					TotalGB: bytesToGB(u.Total),
					UsedGB:  bytesToGB(u.Used),
					FreeGB:  bytesToGB(u.Free),
					Percent: u.UsedPercent,
				})
			}
		}
	}

	if counters, err := disk.IOCounters(); err == nil {
		var rb, wb uint64
		for _, c := range counters {
			rb += c.ReadBytes
			wb += c.WriteBytes
		}
		s.DiskIO = DiskIOStats{ReadBytes: rb, WriteBytes: wb}
	}

	s.Network.TailscaleIP = detectTailscaleIP()
	if counters, err := gopsnet.IOCounters(true); err == nil {
		for _, c := range counters {
			if c.Name == "lo" || strings.HasPrefix(c.Name, "veth") ||
				strings.HasPrefix(c.Name, "br-") || c.Name == "docker0" {
				continue
			}
			s.Network.Interfaces = append(s.Network.Interfaces, NetIface{
				Name:      c.Name,
				BytesSent: c.BytesSent,
				BytesRecv: c.BytesRecv,
			})
		}
	}

	if la, err := load.Avg(); err == nil {
		s.LoadAvg = LoadAvgStats{Load1: la.Load1, Load5: la.Load5, Load15: la.Load15}
	}

	s.Containers = getContainers()
	s.Temps = collectTemps()
	cacheMu.RLock()
	s.TopProcs = cachedProcs
	s.Conns = cachedConns
	cacheMu.RUnlock()
	return s
}

func getContainers() []ContainerStats {
	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil
	}
	defer cli.Close()

	list, err := cli.ContainerList(context.Background(), container.ListOptions{All: true})
	if err != nil {
		return nil
	}

	result := make([]ContainerStats, 0, len(list))
	for _, c := range list {
		name := "unknown"
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		image := c.Image
		if idx := strings.Index(image, "@sha256:"); idx != -1 {
			image = image[:idx]
		}
		result = append(result, ContainerStats{
			ID:     c.ID[:12],
			Name:   name,
			Image:  image,
			State:  c.State,
			Status: c.Status,
		})
	}
	return result
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	json.NewEncoder(w).Encode(collectStats())
}

func main() {
	startBackgroundCollectors()
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/", http.FileServer(http.FS(sub)))
	http.HandleFunc("/api/stats", statsHandler)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	bind := os.Getenv("BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	addr := bind + ":" + port
	log.Printf("Dashboard running on %s",addr)
	log.Fatal(http.ListenAndServe(addr,nil))
}
