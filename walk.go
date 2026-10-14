package main

// walk.go - motor de varredura paralela (sem admin).
//
// Peças combinadas:
//   - gdu (Go): fila-work + workers por núcleo; junction NÃO seguida (folha)
//   - diskus (Rust): soma contínua sem estruturas pesadas
//   - dua-cli (Rust): contagem de itens (arquivos) por diretório
//   - dust (Rust): agregação por profundidade (via DirNode.Depth)
//   - WizTree/Everything (conceito): enumeração em LOTE - aqui
//     GetFileInformationByHandleEx(FileIdBothDirectoryInfo) devolve
//     nome+tamanho+atributos de centenas de entradas por chamada, sem stat()
//     por arquivo (o maior ganho de velocidade sobre FindFirstFile/FindNextFile).
//
// Concorrência: canal-fila + WaitGroup de jobs; pai cria nós dos filhos ANTES
// de liberar seu próprio job -> invariante pai.idx < filho.idx mantida.

import (
	"bufio"
	"encoding/binary"
	"encoding/csv"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type walkJob struct {
	idx  int32
	path string
}

type walkers struct {
	root    string
	workers int
	topN    int
	arena   dirArena
	queue   chan walkJob
	jobs    sync.WaitGroup
	workWG  sync.WaitGroup
	start   time.Time

	files, dirs, bytes, alloc, reparse, errors int64 // atômicos

	errMu   sync.Mutex
	errList []string

	filesCSV bool
	ws       []*walkWorker
}

type walkWorker struct {
	id  int
	buf []byte
	top *topK
	ext map[string]*extStat
	fw  *bufio.Writer
	cw  *csv.Writer
	f   *os.File
	row []string

	// contadores LOCAIS: antes eram 4 atomic.AddInt64 compartilhados por
	// diretório (ping-pong de cache line entre 28 núcleos). Agora acumulam
	// aqui e são despejados em lote nos contadores globais.
	lFiles, lBytes, lAlloc, lDirs int64
}

// flush - despeja os contadores locais do worker nos globais (em lote).
func (w *walkers) flush(ww *walkWorker) {
	if ww.lFiles|ww.lBytes|ww.lAlloc|ww.lDirs == 0 {
		return
	}
	atomic.AddInt64(&w.files, ww.lFiles)
	atomic.AddInt64(&w.bytes, ww.lBytes)
	atomic.AddInt64(&w.alloc, ww.lAlloc)
	atomic.AddInt64(&w.dirs, ww.lDirs)
	ww.lFiles, ww.lBytes, ww.lAlloc, ww.lDirs = 0, 0, 0, 0
}

func newWalkers(root string, workers, topN int, filesCSV bool, shardFn func(i int) string) *walkers {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	w := &walkers{
		root:     root,
		workers:  workers,
		topN:     topN,
		queue:    make(chan walkJob, 1<<16),
		filesCSV: filesCSV,
	}
	w.ws = make([]*walkWorker, workers)
	for i := 0; i < workers; i++ {
		ww := &walkWorker{
			id:  i,
			buf: make([]byte, 256<<10),
			top: newTopK(topN),
			ext: newExtMap(),
		}
		if filesCSV && shardFn != nil {
			f, err := os.Create(shardFn(i))
			if err == nil {
				ww.f = f
				ww.fw = bufio.NewWriterSize(f, 1<<20)
				ww.cw = csv.NewWriter(ww.fw)
				ww.row = make([]string, 3)
			}
		}
		w.ws[i] = ww
	}
	return w
}

func (w *walkers) progress(stop chan struct{}) {
	last := time.Now()
	var lastFiles int64
	for {
		select {
		case <-stop:
			return
		case <-time.After(2000 * time.Millisecond):
			nf := atomic.LoadInt64(&w.files)
			nd := atomic.LoadInt64(&w.dirs)
			nb := atomic.LoadInt64(&w.bytes)
			dt := time.Since(last).Seconds()
			rate := float64(nf-lastFiles) / dt
			fmt.Fprintf(os.Stderr, "\r  %s arqs | %s dirs | %s | %.0f arqs/s      ",
				fmtInt(nf), fmtInt(nd), fmtBytes(nb), rate)
			last = time.Now()
			lastFiles = nf
		}
	}
}

func (w *walkers) run() {
	w.start = time.Now()
	rootIdx := w.arena.addRoot(w.root)

	stop := make(chan struct{})
	go w.progress(stop)

	for _, ww := range w.ws {
		w.workWG.Add(1)
		go func(ww *walkWorker) {
			defer w.workWG.Done()
			defer w.flush(ww)
			for j := range w.queue {
				w.scanDir(ww, j)
				w.jobs.Done()
			}
		}(ww)
	}

	w.jobs.Add(1)
	w.queue <- walkJob{rootIdx, w.root}
	go func() {
		w.jobs.Wait()
		close(w.queue)
	}()

	w.workWG.Wait()
	close(stop)

	for _, ww := range w.ws {
		if ww.cw != nil {
			ww.cw.Flush()
			ww.fw.Flush()
			ww.f.Close()
		}
	}
	w.arena.aggregate()
}

func (w *walkers) recErr(path string, err error) {
	atomic.AddInt64(&w.errors, 1)
	w.errMu.Lock()
	if len(w.errList) < 500 {
		w.errList = append(w.errList, path+": "+err.Error())
	}
	w.errMu.Unlock()
}

// scanDir - enumera um diretório em lotes e agenda os subdirs.
func (w *walkers) scanDir(ww *walkWorker, j walkJob) {
	ww.lDirs++
	if ww.lDirs&511 == 0 {
		w.flush(ww) // progresso visível sem atomics no caminho quente
	}

	h, err := openDir(j.path)
	if err != nil {
		w.recErr(j.path, err)
		w.arena.setDirect(j.idx, 0, 0, 0)
		return
	}
	defer closeHandle(h)

	var dSize, dAlloc, dFiles int64
	var subNames []string

	for {
		n, err := getDirEntries(h, ww.buf)
		if err != nil {
			w.recErr(j.path, err)
			break
		}
		if n == 0 {
			break
		}
		off := 0
		for {
			if off+90 > len(ww.buf) {
				break
			}
			e0 := ww.buf[off:]
			next := int(binary.LittleEndian.Uint32(e0[0:]))
			eof := int64(binary.LittleEndian.Uint64(e0[40:]))
			alloc := int64(binary.LittleEndian.Uint64(e0[48:]))
			attrs := binary.LittleEndian.Uint32(e0[56:])
			nameLen := int(binary.LittleEndian.Uint32(e0[60:]))
			tag := binary.LittleEndian.Uint32(e0[68:])
			if nameLen > 0 && 88+nameLen <= len(e0) {
				name := utf16Bytes(e0[88 : 88+nameLen])
				if name != "." && name != ".." {
					if attrs&fileAttributeDirectory != 0 {
						// Só NÃO desce em links reais (name surrogate: junction/symlink).
						// Cloud (OneDrive), WOF, WCI/container, dedup: diretórios
						// normais -> descer (paridade com gdu/Go stdlib).
						if attrs&fileAttributeReparse != 0 && isNameSurrogate(tag) {
							atomic.AddInt64(&w.reparse, 1)
						} else {
							subNames = append(subNames, name)
						}
					} else {
						dFiles++
						dSize += eof
						dAlloc += alloc
						ww.top.offer(eof, j.path, name, -1)
						addExt(ww.ext, name, eof)
						if ww.cw != nil {
							ww.row[0] = strconv.FormatInt(eof, 10)
							ww.row[1] = name
							ww.row[2] = joinPath(j.path, name)
							ww.cw.Write(ww.row)
						}
					}
				}
			}
			if next == 0 {
				break
			}
			off += next
		}
	}

	ww.lFiles += dFiles
	ww.lBytes += dSize
	ww.lAlloc += dAlloc
	w.arena.setDirect(j.idx, dSize, dAlloc, dFiles)

	if len(subNames) > 0 {
		first := w.arena.addBatch(j.idx, w.arena.depthOf(j.idx)+1, subNames)
		n := len(subNames)
		w.jobs.Add(n)
		for i, nm := range subNames {
			w.queue <- walkJob{idx: first + int32(i), path: joinPath(j.path, nm)}
		}
	}
}

// ---- coleta final ----

func (w *walkers) mergeTop() []fileEnt {
	total := 0
	for _, ww := range w.ws {
		total += len(ww.top.items)
	}
	all := newTopK(total)
	for _, ww := range w.ws {
		for _, e := range ww.top.items {
			all.offer(e.Size, e.Dir, e.Name, e.Rec)
		}
	}
	return all.sorted()
}

func (w *walkers) mergeExt() map[string]*extStat {
	m := newExtMap()
	for _, ww := range w.ws {
		mergeExt(m, ww.ext)
	}
	return m
}

// tidyName - normaliza nome para exibição (sem control chars).
func tidyName(s string) string {
	if !strings.ContainsAny(s, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x0b\x0c\x0e\x0f") {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
}
