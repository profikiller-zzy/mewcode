package memory

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryHeader 是扫描到的一个记忆文件的元数据。
type MemoryHeader struct {
	Filename    string     // 相对于 memoryDir 的路径
	FilePath    string     // 绝对路径
	Scope       string     // "user" 或 "project"；不关心作用域的调用方可为空
	MtimeMs     int64      // 修改时间，单位为 Unix epoch 毫秒
	Description string     // frontmatter 描述；缺失时为空
	Type        MemoryType // frontmatter 类型；无法识别时为空
}

// MaxMemoryFiles 限制向模型展示的记忆数量。
// FrontmatterMaxLines 限制解析文件头时读取的行数。
const (
	MaxMemoryFiles      = 200
	FrontmatterMaxLines = 30
)

// ScanMemoryFiles 扫描记忆目录中的 .md 文件，读取 frontmatter，
// 返回按最新修改时间倒序排列的文件头列表，并限制为 MaxMemoryFiles 个。
// 它同时被查询时召回和提取器使用，提取器可直接使用列表而无需额外执行 ls。
//
// 单次扫描：读取内容时同时获取 mtime，采用“读完再排序”而不是逐个 stat。
// 单文件错误会静默丢弃，避免无法打开一个文件导致整个扫描失败。
func ScanMemoryFiles(ctx context.Context, memoryDir string, scope string) ([]MemoryHeader, error) {
	var mdFiles []string
	walkErr := filepath.WalkDir(memoryDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".md") || name == AutoMemEntrypointName {
			return nil
		}
		mdFiles = append(mdFiles, path)
		return nil
	})
	if walkErr != nil {
		return nil, nil
	}

	results := make([]MemoryHeader, 0, len(mdFiles))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, filePath := range mdFiles {
		if err := ctx.Err(); err != nil {
			break
		}
		wg.Add(1)
		go func(fp string) {
			defer wg.Done()
			hdr, ok := readMemoryHeader(fp, memoryDir)
			if !ok {
				return
			}
			hdr.Scope = scope
			mu.Lock()
			results = append(results, hdr)
			mu.Unlock()
		}(filePath)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		return results[i].MtimeMs > results[j].MtimeMs
	})
	if len(results) > MaxMemoryFiles {
		results = results[:MaxMemoryFiles]
	}
	return results, nil
}

func readMemoryHeader(filePath, memoryDir string) (MemoryHeader, bool) {
	info, err := os.Stat(filePath)
	if err != nil {
		return MemoryHeader{}, false
	}
	f, err := os.Open(filePath)
	if err != nil {
		return MemoryHeader{}, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var sb strings.Builder
	for i := 0; i < FrontmatterMaxLines && scanner.Scan(); i++ {
		sb.WriteString(scanner.Text())
		sb.WriteByte('\n')
	}

	mf := parseFrontmatter(sb.String())
	rel, err := filepath.Rel(memoryDir, filePath)
	if err != nil {
		rel = filepath.Base(filePath)
	}
	return MemoryHeader{
		Filename:    rel,
		FilePath:    filePath,
		MtimeMs:     info.ModTime().UnixMilli(),
		Description: mf.Description,
		Type:        mf.Type,
	}, true
}

// FormatMemoryManifest 将记忆头格式化为文本清单：每个文件一行，包含
// [type]、文件名、时间戳和描述，供召回选择器及提取器 Prompt 使用。
func FormatMemoryManifest(memories []MemoryHeader) string {
	if len(memories) == 0 {
		return ""
	}
	var b strings.Builder
	for i, m := range memories {
		if i > 0 {
			b.WriteByte('\n')
		}
		var tag string
		if m.Type != "" {
			tag = fmt.Sprintf("[%s] ", m.Type)
		}
		var scope string
		if m.Scope != "" {
			scope = fmt.Sprintf("[%s-scope] ", m.Scope)
		}
		ts := time.UnixMilli(m.MtimeMs).UTC().Format("2006-01-02T15:04:05.000Z")
		path := m.FilePath
		if path == "" {
			path = m.Filename
		}
		if m.Description != "" {
			fmt.Fprintf(&b, "- %s%s%s (%s): %s", scope, tag, path, ts, m.Description)
		} else {
			fmt.Fprintf(&b, "- %s%s%s (%s)", scope, tag, path, ts)
		}
	}
	return b.String()
}

