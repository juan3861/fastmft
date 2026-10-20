package main

// aggregate.go - agregacao da arvore (indices de filhos + DFS + soma reversa).
//
// Extraido de build() para ser REUTILIZADO pelo caminho incremental (F4): depois
// de aplicar o delta do USN Journal nos arrays, o re-scan precisa refazer a
// agregacao sem reler a MFT. E a mesma matematica, uma unica fonte de verdade.
//
// Otimizacao v1.2 (Leaf-Separated Topological Graph Aggregation):
//   Arquivos comuns (recDir == 0, ~85% dos registros da MFT) sao folhas terminais
//   (grau de saida 0: nunca possuem filhos). Em vez de inseri-los em childList e
//   empilha-los no DFS (2,6M nos aleatorios com L3 cache miss), acumulamos os
//   arquivos diretamente no pai durante a varredura linear sequencial (1..n) e
//   construimos childList + DFS EXCLUSIVAMENTE para diretorios (~400k nos, 6,5x
//   menor, cabendo no cache L2/L3).

import (
	"fmt"
	"os"
	"time"
)

const recReach = 0x80 // bit temporario para marcar diretorios alcancaveis a partir da raiz

func (e *mftEngine) aggregate() error {
	n := e.recordCount
	e.orphans = 0
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
	} else {
		clear(e.filesAgg)
	}
	if len(e.subdirsAgg) != n {
		e.subdirsAgg = make([]int32, n)
	} else {
		clear(e.subdirsAgg)
	}
	if len(e.childStart) != n+1 {
		e.childStart = make([]int32, n+1)
	} else {
		clear(e.childStart)
	}

	flags := e.flags
	parents := e.parentU32
	sizeL := e.sizeL
	sizeA := e.sizeA
	filesAgg := e.filesAgg
	childStart := e.childStart

	// Garante idempotencia caso aggregate() seja chamado mais de uma vez sobre os mesmos arrays
	for i := 1; i < n; i++ {
		if flags[i]&recDir != 0 {
			sizeL[i] = 0
			sizeA[i] = 0
		}
	}
	sizeL[rootRecIdx] = 0
	sizeA[rootRecIdx] = 0

	// ---- Passo 1 linear: acumula folhas (arquivos) + coleta apenas diretorios ----
	dirs := make([]int32, 0, n/7+64)
	for i := 1; i < n; i++ {
		f := flags[i]
		if f&recNamed == 0 || i == rootRecIdx {
			continue
		}
		p := int(parents[i])
		if p < 16 && p != rootRecIdx {
			// filho de metadado NTFS ($Extend etc.): fora das estatísticas
			continue
		}
		if p >= n || p == i || flags[p]&(recNamed|recDir) != (recNamed|recDir) {
			e.orphans++
			p = rootRecIdx
			parents[i] = rootRecIdx
		}
		if f&recDir != 0 {
			childStart[p+1]++
			dirs = append(dirs, int32(i))
		} else {
			sizeL[p] += sizeL[i]
			sizeA[p] += sizeA[i]
			filesAgg[p]++
		}
	}

	for i := 1; i <= n; i++ {
		childStart[i] += childStart[i-1]
	}
	e.childList = make([]int32, childStart[n])
	childList := e.childList
	for _, idx := range dirs {
		p := parents[idx]
		pos := childStart[p]
		childList[pos] = idx
		childStart[p] = pos + 1
	}
	copy(childStart[1:], childStart[:n])
	childStart[0] = 0
	mark("child_index")

	// ---- Passo 2: DFS restrito a diretorios + agregacao reversa ----
	order := make([]int32, 0, len(dirs)+1)
	stack := make([]int32, 0, 1024)
	stack = append(stack, rootRecIdx)
	flags[rootRecIdx] |= recReach
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		order = append(order, top)
		for c := childStart[top]; c < childStart[top+1]; c++ {
			ch := childList[c]
			flags[ch] |= recReach
			stack = append(stack, ch)
		}
	}
	if len(order)-1 < len(dirs) {
		for _, d := range dirs {
			if flags[d]&recReach == 0 {
				sizeL[d] = 0
				sizeA[d] = 0
				filesAgg[d] = 0
			}
		}
	}
	flags[rootRecIdx] &^= recReach
	subdirsAgg := e.subdirsAgg
	for i := len(order) - 1; i >= 1; i-- {
		nd := order[i]
		flags[nd] &^= recReach
		p := parents[nd]
		sizeL[p] += sizeL[nd]
		sizeA[p] += sizeA[nd]
		subdirsAgg[p] += subdirsAgg[nd] + 1
		filesAgg[p] += filesAgg[nd]
	}
	mark("dfs_agg")
	if timing {
		rss := peakRSS()
		fmt.Fprintf(os.Stderr, "[timing] %-14s RSS pico=%.1f MB (%.0f B/registro, n=%s)\n",
			"memoria", float64(rss)/(1<<20), float64(rss)/float64(n), fmtInt(int64(n)))
	}
	return nil
}
