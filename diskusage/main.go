package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ==================== 配置区域 ====================

var (
	// 要扫描的根目录
	RootDir = `D:\`

	// 并发 worker 数量
	Workers = 8

	// 仅显示占用最大的 N 个文件夹 (0 = 显示全部)
	TopN = 0

	// 进度文件路径 (断点续扫用, 中断时自动保存)
	ProgressFile = `D:\1codeprojects\go\diskusage\.scan_progress.json`

	// 结果输出路径 (扫描完成后生成, 含 .json 和 .txt 两个文件)
	ResultDir = `D:\1codeprojects\go\diskusage`
)

// ==================== 数据结构 ====================

type FolderSize struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type ScanResult struct {
	RootDir   string       `json:"root_dir"`
	ScanTime  string       `json:"scan_time"`
	Elapsed   string       `json:"elapsed"`
	TotalSize int64        `json:"total_size"`
	TotalDirs int          `json:"total_dirs"`
	Folders   []FolderSize `json:"folders"`
}

type Progress struct {
	RootDir   string           `json:"root_dir"`
	Scanned   map[string]int64 `json:"scanned"`
	TotalDirs int              `json:"total_dirs"`
	SavedAt   string           `json:"saved_at"`
}

// ==================== 进度条 ====================

type ActiveDir struct {
	Path      string
	FileCount int64 // 已扫描文件数 (原子更新)
	Size      int64 // 已累计大小 (原子更新)
}

type ProgressBar struct {
	total      int
	current    int64
	startTime  time.Time
	mu         sync.Mutex
	active     map[string]*ActiveDir // path -> ActiveDir
	done       chan struct{}
}

func NewProgressBar(total int) *ProgressBar {
	return &ProgressBar{
		total:     total,
		startTime: time.Now(),
		active:    make(map[string]*ActiveDir),
		done:      make(chan struct{}),
	}
}

func (pb *ProgressBar) AddActive(path string) {
	pb.mu.Lock()
	pb.active[path] = &ActiveDir{Path: path}
	pb.mu.Unlock()
}

func (pb *ProgressBar) RemoveActive(path string) {
	pb.mu.Lock()
	delete(pb.active, path)
	pb.mu.Unlock()
}

func (pb *ProgressBar) Increment() {
	atomic.AddInt64(&pb.current, 1)
}

// GetActiveDir 返回指定路径的 ActiveDir, 供 walker 回调
func (pb *ProgressBar) GetActiveDir(path string) *ActiveDir {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	return pb.active[path]
}

func (pb *ProgressBar) Start() {
	ticker := time.NewTicker(300 * time.Millisecond)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				pb.render()
			case <-pb.done:
				pb.render()
				fmt.Println()
				return
			}
		}
	}()
}

func (pb *ProgressBar) Stop() {
	close(pb.done)
}

func (pb *ProgressBar) render() {
	cur := int(atomic.LoadInt64(&pb.current))
	pct := float64(cur) / float64(pb.total) * 100
	elapsed := time.Since(pb.startTime)

	// 估算剩余时间
	var eta string
	if cur > 0 {
		avg := elapsed / time.Duration(cur)
		remain := time.Duration(pb.total-cur) * avg
		eta = formatDuration(remain)
	} else {
		eta = "计算中..."
	}

	// 进度条
	barWidth := 30
	filled := int(pct / 100 * float64(barWidth))
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

	// 当前正在扫描的目录
	pb.mu.Lock()
	active := make([]*ActiveDir, 0, len(pb.active))
	for _, ad := range pb.active {
		active = append(active, ad)
	}
	pb.mu.Unlock()

	// 清除当前行并输出
	fmt.Printf("\r\033[K  [%s] %d/%d  %.1f%%  已用 %s  剩余 %s", bar, cur, pb.total, pct,
		formatDuration(elapsed), eta)

	if len(active) > 0 {
		// 按文件数降序排列, 显示前 3 个
		sort.Slice(active, func(i, j int) bool {
			return atomic.LoadInt64(&active[i].FileCount) > atomic.LoadInt64(&active[j].FileCount)
		})
		limit := len(active)
		if limit > 3 {
			limit = 3
		}

		// 每个目录单独一行显示
		lines := make([]string, 0, limit)
		for i := 0; i < limit; i++ {
			ad := active[i]
			fc := atomic.LoadInt64(&ad.FileCount)
			sz := atomic.LoadInt64(&ad.Size)
			name := filepath.Base(ad.Path)
			lines = append(lines, fmt.Sprintf("    ├─ %s: %s 个文件, %s", name, formatCount(fc), humanSize(sz)))
		}
		if len(active) > 3 {
			lines = append(lines, fmt.Sprintf("    └─ 还有 %d 个目录...", len(active)-3))
		} else if len(lines) > 0 {
			// 最后一行用 └─
			lines[len(lines)-1] = strings.Replace(lines[len(lines)-1], "├─", "└─", 1)
		}
		fmt.Printf("\n%s", strings.Join(lines, "\n"))
		// 把光标移回去 (ANSI escape: 上移 N 行)
		fmt.Printf("\033[%dA", len(lines))
	}
}

func formatCount(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// ==================== 进度管理 ====================

func loadProgress() *Progress {
	data, err := os.ReadFile(ProgressFile)
	if err != nil {
		return nil
	}
	var p Progress
	if err := json.Unmarshal(data, &p); err != nil {
		return nil
	}
	if p.RootDir != RootDir {
		return nil
	}
	return &p
}

func saveProgress(p *Progress) {
	p.SavedAt = time.Now().Format("2006-01-02 15:04:05")
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(ProgressFile, data, 0644)
}

// ==================== 结果保存 ====================

func saveResultJSON(result *ScanResult) string {
	ts := time.Now().Format("20060102_150405")
	path := filepath.Join(ResultDir, fmt.Sprintf("scan_result_%s.json", ts))
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return ""
	}
	os.WriteFile(path, data, 0644)
	return path
}

func saveResultTXT(result *ScanResult) string {
	ts := time.Now().Format("20060102_150405")
	path := filepath.Join(ResultDir, fmt.Sprintf("scan_result_%s.txt", ts))
	var b strings.Builder

	b.WriteString(fmt.Sprintf("扫描目录: %s\n", result.RootDir))
	b.WriteString(fmt.Sprintf("扫描时间: %s\n", result.ScanTime))
	b.WriteString(fmt.Sprintf("耗时:     %s\n", result.Elapsed))
	b.WriteString(fmt.Sprintf("总占用:   %s\n\n", humanSize(result.TotalSize)))

	b.WriteString(fmt.Sprintf("%-60s %12s %8s\n", "文件夹路径", "大小", "占比"))
	b.WriteString(strings.Repeat("-", 84) + "\n")

	for _, f := range result.Folders {
		pct := float64(f.Size) / float64(result.TotalSize) * 100
		b.WriteString(fmt.Sprintf("%-60s %12s %7.2f%%\n", f.Path, humanSize(f.Size), pct))
	}

	b.WriteString(strings.Repeat("-", 84) + "\n")
	b.WriteString(fmt.Sprintf("共 %d 个文件夹\n", result.TotalDirs))

	os.WriteFile(path, []byte(b.String()), 0644)
	return path
}

// ==================== 核心逻辑 ====================

func main() {
	start := time.Now()

	// 加载已有进度
	progress := loadProgress()
	if progress != nil {
		fmt.Printf("[进度] 发现上次扫描记录 (%s), 已完成 %d/%d 个目录\n",
			progress.SavedAt, len(progress.Scanned), progress.TotalDirs)
	} else {
		progress = &Progress{
			RootDir: RootDir,
			Scanned: make(map[string]int64),
		}
	}

	// 获取所有子目录
	allDirs, err := listSubDirs(RootDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取目录失败: %v\n", err)
		os.Exit(1)
	}
	progress.TotalDirs = len(allDirs)

	// 过滤出尚未扫描的目录
	var pending []string
	for _, d := range allDirs {
		if _, done := progress.Scanned[d]; !done {
			pending = append(pending, d)
		}
	}

	alreadyDone := len(progress.Scanned)
	fmt.Printf("扫描目录: %s\n", RootDir)
	fmt.Printf("共 %d 个子目录, 已完成 %d, 待扫描 %d, 并发 %d\n\n",
		len(allDirs), alreadyDone, len(pending), Workers)

	if len(pending) == 0 {
		fmt.Println("所有目录均已扫描完毕, 无需继续")
		printResults(progress)
		return
	}

	// 注册中断信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	var mu sync.Mutex
	interrupted := false

	go func() {
		<-sigCh
		mu.Lock()
		interrupted = true
		mu.Unlock()

		fmt.Printf("\n\n[中断] 正在保存进度...\n")
		mu.Lock()
		saveProgress(progress)
		mu.Unlock()
		fmt.Printf("[中断] 进度已保存至: %s\n", ProgressFile)
		fmt.Printf("[中断] 下次运行将自动从断点继续\n")
		os.Exit(0)
	}()

	// 初始化进度条
	bar := NewProgressBar(alreadyDone + len(pending))
	bar.current = int64(alreadyDone)
	bar.Start()

	// 并发扫描
	jobs := make(chan string, len(pending))
	results := make(chan FolderSize, len(pending))

	var wg sync.WaitGroup
	for i := 0; i < Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				bar.AddActive(path)
				size := dirSizeWithProgress(path, bar)
				bar.RemoveActive(path)
				results <- FolderSize{Path: path, Size: size}
			}
		}()
	}

	go func() {
		for _, d := range pending {
			jobs <- d
		}
		close(jobs)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	// 收集结果
	completed := 0
	saveCounter := 0
	for r := range results {
		mu.Lock()
		progress.Scanned[r.Path] = r.Size
		completed++
		saveCounter++
		if saveCounter%10 == 0 {
			saveProgress(progress)
		}
		mu.Unlock()

		bar.Increment()

		mu.Lock()
		isInterrupted := interrupted
		mu.Unlock()
		if isInterrupted {
			return
		}
	}

	bar.Stop()
	elapsed := time.Since(start)

	// 构建并保存结果
	result := buildResult(progress, elapsed)
	jsonPath := saveResultJSON(result)
	txtPath := saveResultTXT(result)

	// 清理进度文件
	os.Remove(ProgressFile)

	printResults(progress)
	fmt.Printf("\n扫描完成! 耗时: %v\n", elapsed.Round(time.Millisecond))
	fmt.Printf("结果已保存:\n")
	fmt.Printf("  JSON: %s\n", jsonPath)
	fmt.Printf("  TXT:  %s\n", txtPath)
}

// dirSizeWithProgress 带子目录进度回调的目录大小计算
func dirSizeWithProgress(path string, bar *ProgressBar) int64 {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
			// 回调进度条: 更新文件计数和累计大小
			if ad := bar.GetActiveDir(path); ad != nil {
				atomic.AddInt64(&ad.FileCount, 1)
				atomic.StoreInt64(&ad.Size, size)
			}
		}
		return nil
	})
	if err != nil {
		return -1
	}
	return size
}

func buildResult(p *Progress, elapsed time.Duration) *ScanResult {
	var folders []FolderSize
	var totalSize int64
	for path, size := range p.Scanned {
		if size >= 0 {
			folders = append(folders, FolderSize{Path: path, Size: size})
			totalSize += size
		}
	}
	sort.Slice(folders, func(i, j int) bool {
		return folders[i].Size > folders[j].Size
	})
	return &ScanResult{
		RootDir:   p.RootDir,
		ScanTime:  time.Now().Format("2006-01-02 15:04:05"),
		Elapsed:   elapsed.Round(time.Millisecond).String(),
		TotalSize: totalSize,
		TotalDirs: len(folders),
		Folders:   folders,
	}
}

// ==================== 输出 ====================

func printResults(p *Progress) {
	var folders []FolderSize
	var totalSize int64
	for path, size := range p.Scanned {
		if size >= 0 {
			folders = append(folders, FolderSize{Path: path, Size: size})
			totalSize += size
		}
	}

	sort.Slice(folders, func(i, j int) bool {
		return folders[i].Size > folders[j].Size
	})

	fmt.Printf("\n%-60s %12s %8s\n", "文件夹路径", "大小", "占比")
	fmt.Println(strings.Repeat("-", 84))

	limit := len(folders)
	if TopN > 0 && TopN < limit {
		limit = TopN
	}

	for i := 0; i < limit; i++ {
		f := folders[i]
		pct := float64(f.Size) / float64(totalSize) * 100
		fmt.Printf("%-60s %12s %7.2f%%\n", f.Path, humanSize(f.Size), pct)
	}

	fmt.Println(strings.Repeat("-", 84))
	fmt.Printf("总占用: %s\n", humanSize(totalSize))
}

// ==================== 工具函数 ====================

func humanSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)
	switch {
	case bytes >= TB:
		return fmt.Sprintf("%.2f TB", float64(bytes)/float64(TB))
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func listSubDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs, nil
}
