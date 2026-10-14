package main

// aggregate.go - agregacao da arvore (indices de filhos + DFS + soma reversa).
//
// Extraido de build() para ser REUTILIZADO pelo caminho incremental (F4): depois
// de aplicar o delta do USN Journal nos arrays, o re-scan precisa refazer a
// agregacao sem reler a MFT. E a mesma matematica, uma unica fonte de verdade.

import (
	"fmt"
	"os"
	"time"
)

func (e *mftEngine) aggregate() error {
	n := e.recordCount
	e.orphans = 0
	// AETHERSCAN_TIMING=1 imprime o custo de cada fase (mesmo formato do build()).
	timing := os.Getenv("AETHERSCAN_TIMING") != ""
	tPhase := time.Now()
	mark := func(name string) {
		if !timing {
			return
		}
		now := time.Now()
		fmt.Fprintf(os.Stderr, "[timing] %-14s %7.3fs\n", name, now.Sub(tPhase).Seconds())
		tPhase = now
	}
	if len(e.filesAgg) != n {
		e.filesAgg = make([]int32, n)
	}
	if len(e.subdirsAgg) != n {
		e.subdirsAgg = make([]int32, n)
	}
	for i := range e.filesAgg {
		e.filesAgg[i] = 0
	}
	for i := range e.subdirsAgg {
		e.subdirsAgg[i] = 0
	}
	// ---- índices de filhos (counting sort) ----
	e.childStart = make([]int32, n+1)
	for i := 1; i < n; i++ {
		if e.flags[i]&recNamed == 0 || i == rootRecIdx {
			continue
		}
		p := int(e.parentU32[i])
		if int(p) < 16 && int(p) != rootRecIdx {
			// filho de metadado NTFS ($Extend etc.): fora das estatísticas
			continue
		}
		if p < n && p != i && e.flags[p]&(recNamed|recDir) == (recNamed|recDir) {
			e.childStart[p+1]++
		} else {
			e.orphans++
			e.parentU32[i] = rootRecIdx
			e.childStart[rootRecIdx+1]++
		}
	}
	for i := 1; i <= n; i++ {
		e.childStart[i] += e.childStart[i-1]
	}
	e.childList = make([]int32, e.childStart[n])
	fill := make([]int32, n)
	copy(fill, e.childStart[:n])
	for i := 1; i < n; i++ {
		if e.flags[i]&recNamed == 0 || i == rootRecIdx {
			continue
		}
		p := int(e.parentU32[i])
		if int(p) < 16 && int(p) != rootRecIdx {
			continue
		}
		e.childList[fill[p]] = int32(i)
		fill[p]++
	}
	mark("child_index")

	// ---- DFS a partir da raiz + agregação reversa ----
	order := make([]int32, 0, e.inUse)
	stack := make([]int32, 0, 1024)
	stack = append(stack, rootRecIdx)
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		order = append(order, top)
		for c := e.childStart[top]; c < e.childStart[top+1]; c++ {
			stack = append(stack, e.childList[c])
		}
	}
	for i := len(order) - 1; i >= 1; i-- {
		nd := order[i]
		p := e.parentU32[nd]
		e.sizeL[p] += e.sizeL[nd]
		e.sizeA[p] += e.sizeA[nd]
		if e.flags[nd]&recDir != 0 {
			e.subdirsAgg[p] += e.subdirsAgg[nd] + 1
			e.filesAgg[p] += e.filesAgg[nd]
		} else {
			e.filesAgg[p]++
		}
	}
	mark("dfs_agg")
	if timing {
		rss := peakRSS()
		fmt.Fprintf(os.Stderr, "[timing] %-14s RSS pico=%.1f MB (%.0f B/registro, n=%s)\n",
			"memoria", float64(rss)/(1<<20), float64(rss)/float64(n), fmtInt(int64(n)))
	}
	return nil
}
