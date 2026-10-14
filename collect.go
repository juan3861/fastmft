package main

// collect.go - coletores por worker: top-K de arquivos, estatísticas por extensão.
// Peça do top-K: min-heap artesanal (sem container/heap) - evita interface boxing
// no caminho quente de 8M+ arquivos.

import (
	"sort"
	"strings"
)

type fileEnt struct {
	Size int64
	Dir  string // dir de origem (walk) - vazio no modo mft
	Name string
	Rec  int64 // record MFT do dir pai (modo mft; -1 = não usar)
}

type topK struct {
	k     int
	items []fileEnt
}

func newTopK(k int) *topK {
	if k < 0 {
		k = 0
	}
	return &topK{k: k, items: make([]fileEnt, 0, k+1)}
}

// offer - O(log k) no pior caso, mas tipicamente O(1) (compara com o mínimo).
func (t *topK) offer(size int64, dir, name string, rec int64) {
	if t.k <= 0 {
		return
	}
	if len(t.items) < t.k {
		t.items = append(t.items, fileEnt{size, dir, name, rec})
		// sift-up
		i := len(t.items) - 1
		for i > 0 {
			p := (i - 1) / 2
			if t.items[p].Size <= t.items[i].Size {
				break
			}
			t.items[p], t.items[i] = t.items[i], t.items[p]
			i = p
		}
		return
	}
	if size <= t.items[0].Size {
		return
	}
	t.items[0] = fileEnt{size, dir, name, rec}
	// sift-down
	i := 0
	n := len(t.items)
	for {
		l, r := 2*i+1, 2*i+2
		m := i
		if l < n && t.items[l].Size < t.items[m].Size {
			m = l
		}
		if r < n && t.items[r].Size < t.items[m].Size {
			m = r
		}
		if m == i {
			break
		}
		t.items[m], t.items[i] = t.items[i], t.items[m]
		i = m
	}
}

// sorted - devolve em ordem decrescente (K pequeno: insertion sort).
func (t *topK) sorted() []fileEnt {
	out := make([]fileEnt, len(t.items))
	copy(out, t.items)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Size > out[j-1].Size; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

type extStat struct {
	Count int64
	Bytes int64
}

func newExtMap() map[string]*extStat { return make(map[string]*extStat, 256) }

// addExt - registra (ext, tamanho). Extração sem alocação no caminho comum.
func addExt(m map[string]*extStat, name string, size int64) {
	i := strings.LastIndexByte(name, '.')
	if i < 0 || i == len(name)-1 {
		s := m["(sem)"]
		if s == nil {
			s = &extStat{}
			m["(sem)"] = s
		}
		s.Count++
		s.Bytes += size
		return
	}
	ext := name[i:]
	if len(ext) > 16 { // extensões absurdas -> cai em "(longa)"
		ext = "(longa)"
	} else if ext != strings.ToLower(ext) {
		ext = strings.ToLower(ext)
	}
	s := m[ext]
	if s == nil {
		s = &extStat{}
		m[ext] = s
	}
	s.Count++
	s.Bytes += size
}

func mergeExt(dst, src map[string]*extStat) {
	for k, v := range src {
		if d, ok := dst[k]; ok {
			d.Count += v.Count
			d.Bytes += v.Bytes
		} else {
			dst[k] = v
		}
	}
}

// ---- acumulador de extensões SEM alocação (caminho MFT) ----
//
// O caminho MFT precisa agregar por extensão para 2,2 M arquivos em C:. Converter
// cada nome para string custaria 2,2 M alocações so para jogar no mapa. Aqui a
// chave e um array fixo (extensao <= 15 bytes) — zero alocacao no caminho quente;
// as strings so nascem nas ~centenas de extensoes distintas, no relatorio.

type extKey struct {
	b [16]byte
	n uint8
}

func (k extKey) String() string { return string(k.b[:k.n]) }

// extKeyOfUTF16 - extrai a chave de extensao de um nome em UTF-16LE.
func extKeyOfUTF16(nameB []byte) extKey {
	var k extKey
	last := -1
	for i := 0; i+1 < len(nameB); i += 2 {
		if nameB[i] == '.' && nameB[i+1] == 0 {
			last = i
		}
	}
	if last < 0 || last == len(nameB)-2 {
		copy(k.b[:], "(sem)")
		k.n = 5
		return k
	}
	ext := nameB[last:]
	if len(ext)/2 > 16 {
		copy(k.b[:], "(longa)")
		k.n = 7
		return k
	}
	o := 0
	for i := 0; i+1 < len(ext); i += 2 {
		if ext[i+1] != 0 {
			copy(k.b[:], "(unicode)")
			k.n = 9
			return k
		}
		c := ext[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		k.b[o] = c
		o++
	}
	k.n = uint8(o)
	return k
}

// extTopKey - top-K de extensoes, convertendo chaves para string so no fim.
func extTopKey(m map[extKey]*extStat, k int) []ExtRow {
	rows := make([]ExtRow, 0, len(m))
	for key, s := range m {
		rows = append(rows, ExtRow{Ext: key.String(), Count: s.Count, Bytes: s.Bytes})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Bytes > rows[j].Bytes })
	if len(rows) > k {
		rows = rows[:k]
	}
	return rows
}
