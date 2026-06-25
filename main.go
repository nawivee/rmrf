package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	filesDeleted atomic.Int64
	bytesFreed   atomic.Int64
	startTime    time.Time
	targetDir    string
	done         atomic.Bool
	deleteErr    atomic.Value
	printMu      sync.Mutex
	lastPrint    time.Time

	sem     chan struct{}
	silence bool
)

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}

func main() {
	defaultConcurrency := runtime.NumCPU() * 4
	if rotational, err := isRotational(os.Args[len(os.Args)-1]); err == nil && rotational {
		defaultConcurrency = 1
	}
	concurrency := flag.Int("c", defaultConcurrency, "max concurrent directory goroutines (auto: 1 for HDD, NumCPU*4 for SSD)")
	silent := flag.Bool("s", false, "silent mode, no output")
	port := flag.Int("port", 8698, "HTTP progress server port")
	noHTTP := flag.Bool("no-http", false, "disable HTTP progress server (overrides -port)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "rmrf %s\nusage: rmrf [flags] <directory>\n", version())
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}
	dir := flag.Arg(0)
	sem = make(chan struct{}, *concurrency)

	info, err := os.Stat(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if !info.IsDir() {
		fmt.Fprintln(os.Stderr, "error: path is not a directory")
		os.Exit(1)
	}

	startTime = time.Now()
	targetDir = dir
	silence = *silent
	httpEnabled := !*noHTTP
	if httpEnabled {
		go func() {
			http.HandleFunc("/", handleStatus)
			http.ListenAndServe(fmt.Sprintf(":%d", *port), nil)
		}()
	}

	if !*silent {
		if httpEnabled {
			fmt.Printf("Progress available at http://localhost:%d\n", *port)
		}
		fmt.Printf("Deleting %s...\n", dir)
	}

	if err := removeDir(dir); err != nil {
		deleteErr.Store(err.Error())
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		done.Store(true)
		os.Exit(1)
	}

	done.Store(true)
	if !*silent {
		elapsed := time.Since(startTime).Round(time.Millisecond)
		fmt.Printf("\rDone: %d files deleted, %s freed in %s\n",
			filesDeleted.Load(), formatBytes(bytesFreed.Load()), elapsed)
	}
}

func removeDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	var firstErr atomic.Value

	for {
		entries, err := f.ReadDir(256)
		for _, entry := range entries {
			full := filepath.Join(path, entry.Name())
			if entry.IsDir() {
				select {
				case sem <- struct{}{}:
					wg.Add(1)
					go func(p string) {
						defer wg.Done()
						defer func() { <-sem }()
						if err := removeDir(p); err != nil && firstErr.Load() == nil {
							firstErr.Store(err)
						}
					}(full)
				default:
					// sem full — process inline to avoid deadlock
					if err := removeDir(full); err != nil && firstErr.Load() == nil {
						firstErr.Store(err)
					}
				}
			} else {
				info, _ := entry.Info()
				if info != nil {
					bytesFreed.Add(info.Size())
				}
				if err := os.Remove(full); err != nil {
					if firstErr.Load() == nil {
						firstErr.Store(err)
					}
				} else {
					filesDeleted.Add(1)
					printProgress()
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			f.Close()
			return err
		}
	}
	f.Close()

	wg.Wait()

	if e := firstErr.Load(); e != nil {
		return e.(error)
	}
	return os.Remove(path)
}

func printProgress() {
	if silence {
		return
	}
	printMu.Lock()
	defer printMu.Unlock()
	if time.Since(lastPrint) < 200*time.Millisecond {
		return
	}
	fmt.Printf("\r  %d files deleted, %s freed", filesDeleted.Load(), formatBytes(bytesFreed.Load()))
	lastPrint = time.Now()
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	files := filesDeleted.Load()
	freed := bytesFreed.Load()
	elapsed := time.Since(startTime).Round(time.Second)

	status := "in_progress"
	if done.Load() {
		status = "done"
		if e := deleteErr.Load(); e != nil {
			status = "error: " + e.(string)
		}
	}

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "status:  %s\ndir:     %s\nstarted: %s\nfiles:   %d\nfreed:   %s\nelapsed: %s\n",
		status, targetDir, startTime.Format(time.RFC3339), files, formatBytes(freed), elapsed)
}

// isRotational checks /sys/dev/block to detect HDD vs SSD.
func isRotational(path string) (bool, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false, err
	}
	major := (st.Dev >> 8) & 0xfff
	minor := (st.Dev & 0xff) | ((st.Dev >> 12) & 0xfff00)

	sysPath := fmt.Sprintf("/sys/dev/block/%d:%d", major, minor)
	resolved, err := filepath.EvalSymlinks(sysPath)
	if err != nil {
		return false, err
	}

	rotPath := filepath.Join(resolved, "queue/rotational")
	if _, err := os.Stat(rotPath); os.IsNotExist(err) {
		// partition — go up to the parent disk
		rotPath = filepath.Join(filepath.Dir(resolved), "queue/rotational")
	}

	data, err := os.ReadFile(rotPath)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(data)) == "1", nil
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
