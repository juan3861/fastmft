package main

// report.go - modelo de saída e exportação (JSON + CSV + console).
// Peça "gdu": formato de sumário com top-dirs/top-files/extensões,
// porém mais leve (sem árvore completa em JSON) e com contagens de reparse.

import (
	"bufio"
	"encoding/csv"
	"os"
	"sort"
	"strconv"
	"sync/atomic"
	"time"
)

type DirInfo struct {
	Size    int64  `json:"size"`
	Alloc   int64  `json:"alloc"`
	Files   int64  `json:"files"`
	Subdirs int64  `json:"subdirs"`
	Path    string `json:"path"`
	Depth   uint16 `json:"depth"`
}

type NameSize struct {
	Size int64  `json:"size"`
	Path string `json:"path"`
}

type ExtRow struct {
	Ext   string `json:"ext"`
	Count int64  `json:"count"`
	Bytes int64  `json:"bytes"`
}

type Report struct {
	Engine   string     `json:"engine"`
	Root     string     `json:"root"`
	Started  string     `json:"started"`
	Duration float64    `json:"duration_s"`
	Files    int64      `json:"files"`
	Dirs     int64      `json:"dirs"`
	Reparse  int64      `json:"reparse_skipped"`
	Errors   int64      `json:"errors"`
	Bytes    int64      `json:"bytes"`
	Alloc    int64      `json:"alloc"`
	Deleted  int64      `json:"deleted_records,omitempty"`
	Unnamed  int64      `json:"unnamed_records,omitempty"`
	Orphans  int64      `json:"orphans,omitempty"`
	ExtRecs  int64      `json:"extension_records,omitempty"`
	MftSegs  int        `json:"mft_data_extents,omitempty"`
	MftRuns  int        `json:"mft_runs,omitempty"`
	// MftDataBytes - tamanho real do $DATA do $MFT; MftRunBytes - bytes cobertos
	// pela runlist montada; RecsRead/RecsTotal - registros lidos/esperados.
	MftDataBytes int64      `json:"mft_data_bytes,omitempty"`
	MftRunBytes  int64      `json:"mft_run_bytes,omitempty"`
	RecsRead     int        `json:"records_read,omitempty"`
	RecsTotal    int        `json:"records_total,omitempty"`
	ErrList      []string   `json:"error_samples,omitempty"`
	TopDirs  []DirInfo  `json:"top_dirs"`
	TopFiles []NameSize `json:"top_files"`
	Exts     []ExtRow   `json:"by_extension"`
}

func extTop(m map[string]*extStat, k int) []ExtRow {
	rows := make([]ExtRow, 0, len(m))
	for ext, s := range m {
		rows = append(rows, ExtRow{Ext: ext, Count: s.Count, Bytes: s.Bytes})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Bytes > rows[j].Bytes })
	if len(rows) > k {
		rows = rows[:k]
	}
	return rows
}

// ---- CSV ----

type csvSink struct {
	f *os.File
	b *bufio.Writer
	w *csv.Writer
}

// createCSV - o csv.Writer embrulha o arquivo num bufio de 4 KB por padrao, o
// que gera MILHARES de write() pequenos ao exportar 400 k diretorios. Aqui o
// buffer e de 1 MB.
func createCSV(path string) (*csvSink, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	b := bufio.NewWriterSize(f, 1<<20)
	return &csvSink{f: f, b: b, w: csv.NewWriter(b)}, nil
}

func (c *csvSink) Write(row []string) error { return c.w.Write(row) }

func (c *csvSink) Close() error {
	c.w.Flush()
	if err := c.w.Error(); err != nil {
		c.f.Close()
		return err
	}
	if err := c.b.Flush(); err != nil {
		c.f.Close()
		return err
	}
	return c.f.Close()
}

// ---- preenchimento do Report (walk) ----

func (w *walkers) fillReport(r *Report) {
	r.Engine = "walk"
	r.Root = w.root
	r.Started = w.start.Format(time.RFC3339)
	r.Duration = time.Since(w.start).Seconds()
	r.Files = atomic.LoadInt64(&w.files)
	r.Dirs = atomic.LoadInt64(&w.dirs)
	r.Reparse = atomic.LoadInt64(&w.reparse)
	r.Errors = atomic.LoadInt64(&w.errors)
	r.Bytes = atomic.LoadInt64(&w.bytes)
	r.Alloc = atomic.LoadInt64(&w.alloc)

	w.errMu.Lock()
	r.ErrList = append([]string{}, w.errList...)
	w.errMu.Unlock()

	// top dirs
	n := w.arena.len()
	sizes := make([]int64, n)
	for i := 0; i < n; i++ {
		sizes[i] = w.arena.at(int32(i)).Size
	}
	t := newIdxTop(w.topN, sizes)
	for i := 1; i < n; i++ {
		t.offer(int32(i))
	}
	paths := make([]string, n)
	if n > 0 {
		paths[0] = w.arena.at(0).Name
	}
	for i := 1; i < n; i++ {
		paths[i] = joinPath(paths[w.arena.at(int32(i)).Parent], w.arena.at(int32(i)).Name)
	}
	for _, i := range t.sortedDesc() {
		nd := w.arena.at(i)
		r.TopDirs = append(r.TopDirs, DirInfo{
			Size:    nd.Size,
			Alloc:   nd.Alloc,
			Files:   nd.Files,
			Subdirs: nd.Subdirs,
			Path:    paths[i],
			Depth:   nd.Depth,
		})
	}

	// top files
	top := w.mergeTop()
	if len(top) > w.topN {
		top = top[:w.topN]
	}
	for _, fe := range top {
		r.TopFiles = append(r.TopFiles, NameSize{Size: fe.Size, Path: joinPath(fe.Dir, fe.Name)})
	}

	r.Exts = extTop(w.mergeExt(), 50)
}

// writeDirsCSV - exporta todos os dirs do walk (ordenado por tamanho).
func (w *walkers) writeDirsCSV(path string) error {
	cw, err := createCSV(path)
	if err != nil {
		return err
	}
	defer cw.Close()
	n := w.arena.len()
	paths := make([]string, n)
	if n > 0 {
		paths[0] = w.arena.at(0).Name
	}
	for i := 1; i < n; i++ {
		paths[i] = joinPath(paths[w.arena.at(int32(i)).Parent], w.arena.at(int32(i)).Name)
	}
	idx := make([]int32, 0, n-1)
	for i := 1; i < n; i++ {
		idx = append(idx, int32(i))
	}
	sort.Slice(idx, func(a, b int) bool {
		return w.arena.at(idx[a]).Size > w.arena.at(idx[b]).Size
	})
	for _, i := range idx {
		nd := w.arena.at(i)
		if err := cw.Write([]string{
			paths[i],
			strconv.FormatInt(nd.Size, 10),
			strconv.FormatInt(nd.Alloc, 10),
			strconv.FormatInt(nd.Files, 10),
			strconv.FormatInt(nd.Subdirs, 10),
		}); err != nil {
			return err
		}
	}
	return nil
}

// writeTopCSV - top files em CSV.
func writeTopCSV(path string, rep *Report) error {
	cw, err := createCSV(path)
	if err != nil {
		return err
	}
	defer cw.Close()
	for _, f := range rep.TopFiles {
		if err := cw.Write([]string{strconv.FormatInt(f.Size, 10), f.Path}); err != nil {
			return err
		}
	}
	return nil
}
