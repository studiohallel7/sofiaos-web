package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// ── CONFIG ───────────────────────────────────────────────────────────────────

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ── METRICS ──────────────────────────────────────────────────────────────────

type Snapshot struct {
	CPU     float64 `json:"cpu_percent"`
	RAMMb   uint64  `json:"ram_used_mb"`
	RAMPct  float64 `json:"ram_percent"`
	DiskGb  uint64  `json:"disk_used_gb"`
	DiskPct float64 `json:"disk_percent"`
	Load1   float64 `json:"load1"`
	Uptime  float64 `json:"uptime_seconds"`
	At      string  `json:"at"`
}

type cpuSample struct{ total, idle uint64 }

func readCPU() (cpuSample, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSample{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var vals []uint64
		for _, v := range fields {
			n, _ := strconv.ParseUint(v, 10, 64)
			vals = append(vals, n)
		}
		var total uint64
		for _, v := range vals {
			total += v
		}
		idle := vals[3]
		if len(vals) > 4 {
			idle += vals[4]
		}
		return cpuSample{total, idle}, nil
	}
	return cpuSample{}, nil
}

func collectCPU() float64 {
	s1, _ := readCPU()
	time.Sleep(200 * time.Millisecond)
	s2, _ := readCPU()
	td := float64(s2.total - s1.total)
	id := float64(s2.idle - s1.idle)
	if td == 0 {
		return 0
	}
	return round2((1 - id/td) * 100)
}

func collectRAM() (uint64, float64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	m := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) >= 2 {
			m[strings.TrimSuffix(parts[0], ":")], _ = strconv.ParseUint(parts[1], 10, 64)
		}
	}
	total := m["MemTotal"]
	free := m["MemFree"] + m["Buffers"] + m["Cached"] + m["SReclaimable"]
	used := total - free
	pct := float64(0)
	if total > 0 {
		pct = round2(float64(used) / float64(total) * 100)
	}
	return used / 1024, pct
}

func collectDisk() (uint64, float64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return 0, 0
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bfree * uint64(st.Bsize)
	used := total - free
	pct := float64(0)
	if total > 0 {
		pct = round2(float64(used) / float64(total) * 100)
	}
	return used / (1024 * 1024 * 1024), pct
}

func collectLoad() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.Fields(string(data))[0], 64)
	return f
}

func collectUptime() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.Fields(string(data))[0], 64)
	return f
}

func round2(f float64) float64 { return float64(int(f*100)) / 100 }

func collectAll() *Snapshot {
	ram, ramPct := collectRAM()
	disk, diskPct := collectDisk()
	return &Snapshot{
		CPU:     collectCPU(),
		RAMMb:   ram,
		RAMPct:  ramPct,
		DiskGb:  disk,
		DiskPct: diskPct,
		Load1:   collectLoad(),
		Uptime:  collectUptime(),
		At:      time.Now().Format(time.RFC3339),
	}
}

// ── TERMINAL ─────────────────────────────────────────────────────────────────

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

func terminalHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("terminal upgrade: %v", err)
		return
	}
	defer conn.Close()

	shell := env("SOFIAOS_SHELL", "/bin/bash")
	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte("\r\n[sofiaos] erro ao iniciar shell\r\n"))
		return
	}
	defer cmd.Process.Kill()

	done := make(chan struct{})

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				conn.WriteMessage(websocket.BinaryMessage, buf[:n])
			}
			if err != nil {
				break
			}
		}
		close(done)
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				conn.WriteMessage(websocket.BinaryMessage, buf[:n])
			}
			if err != nil {
				break
			}
		}
	}()

	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				cmd.Process.Kill()
				return
			}
			io.Writer(stdin).Write(msg)
		}
	}()

	<-done
}

// ── MAIN ─────────────────────────────────────────────────────────────────────

func main() {
	addr := env("SOFIAOS_ADDR", ":8080")
	galeneURL := env("SOFIAOS_GALENE_URL", "https://conference.studiohallel.online:8443")
	staticDir := env("SOFIAOS_STATIC", "./static")

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(collectAll())
	})

	mux.HandleFunc("GET /api/terminal", terminalHandler)

	galene, err := url.Parse(galeneURL)
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/galene/", http.StripPrefix("/galene", httputil.NewSingleHostReverseProxy(galene)))

	mux.Handle("/", http.FileServer(http.Dir(staticDir)))

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("sofiaos em %s  static=%s  galene=%s", addr, staticDir, galeneURL)
	log.Fatal(srv.ListenAndServe())
}