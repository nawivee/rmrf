package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
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

	sem chan struct{}
)

func main() {
	concurrency := flag.Int("c", runtime.NumCPU()*4, "max concurrent directory goroutines")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: rmrf [flags] <directory>")
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
	fmt.Println("Progress available at http://localhost:8698")
	fmt.Printf("Deleting %s...\n", dir)

	go func() {
		http.HandleFunc("/", handleStatus)
		http.ListenAndServe(":8698", nil)
	}()

	if err := removeDir(dir); err != nil {
		deleteErr.Store(err.Error())
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		done.Store(true)
		os.Exit(1)
	}

	done.Store(true)
	elapsed := time.Since(startTime).Round(time.Millisecond)
	fmt.Printf("\rDone: %d files deleted, %s freed in %s\n",
		filesDeleted.Load(), formatBytes(bytesFreed.Load()), elapsed)
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
				wg.Add(1)
				sem <- struct{}{}
				go func(p string) {
					defer wg.Done()
					defer func() { <-sem }()
					if err := removeDir(p); err != nil && firstErr.Load() == nil {
						firstErr.Store(err)
					}
				}(full)
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
