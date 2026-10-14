package main

// node.go - modelo de dados do mapa (árvore de DIRETÓRIOS + agregados).
//
// Decisão de arquitetura (peças combinadas dos campeões):
//   - gdu: exporta árvore completa -> pesado; aqui: só dirs como nós,
//     arquivos viram agregados (soma, contagem, top-K, extensões).
//   - diskus/dua: soma pura é rápida; mantemos a rapidez e adicionamos a árvore.
//   - dust: agregação por profundidade para tree view.
//
// Invariante de ouro: pai.idx < filho.idx (pai é descoberto antes dos filhos)
// => agregação bottom-up é um único loop reverso, O(n), cache-friendly.
//
// CONTENÇÃO (corrigida): antes, addBatch/setDirect/depthOf tomavam um
// sync.Mutex GLOBAL -> 3 locks por diretório, com 28 workers brigando pelo
// mesmo mutex (393k diretórios em C: = ~1,2 M aquisições). Agora cada worker
// RESERVA uma faixa de índices com um AddInt64 e escreve nela SEM lock (faixas
// disjuntas); o mutex só aparece ao criar um bloco novo. O invariante
// pai<filho é preservado porque o pai reserva os índices dos filhos ANTES de
// liberá-los para a fila.

import (
	"strings"
	"sync"
	"sync/atomic"
)

type DirNode struct {
	Parent  int32
	Name    string
	Depth   uint16
	Size    int64 // aparente acumulado (includes subdirs)
	Alloc   int64 // alocado em disco acumulado
	Files   int64 // arquivos recursivos
	Subdirs int64 // subdirs recursivos
}

// arenaChunk - nós por bloco (blocos evitam realocar/copiar um slice gigante).
const arenaChunk = 1 << 16

type dirArena struct {
	mu     sync.Mutex
	count  int64
	chunks [][]DirNode
}

func (a *dirArena) ensure(upTo int) {
	need := upTo/arenaChunk + 1
	a.mu.Lock()
	for len(a.chunks) < need {
		a.chunks = append(a.chunks, make([]DirNode, arenaChunk))
	}
	a.mu.Unlock()
}

func (a *dirArena) at(i int32) *DirNode {
	return &a.chunks[int(i)/arenaChunk][int(i)%arenaChunk]
}

func (a *dirArena) len() int { return int(atomic.LoadInt64(&a.count)) }

// reserve - reserva n índices contíguos de forma atômica (sem lock).
func (a *dirArena) reserve(n int) int32 {
	first := int32(atomic.AddInt64(&a.count, int64(n)) - int64(n))
	a.ensure(int(first) + n)
	return first
}

func (a *dirArena) addRoot(name string) int32 {
	idx := a.reserve(1)
	n := a.at(idx)
	n.Parent = -1
	n.Name = name
	n.Depth = 0
	return idx
}

// addBatch - registra N subdirs do mesmo pai; retorna o índice do primeiro.
// Índices contíguos: [first, first+n). Sem lock (faixa exclusiva).
func (a *dirArena) addBatch(parent int32, depth uint16, names []string) int32 {
	first := a.reserve(len(names))
	for i := range names {
		n := a.at(first + int32(i))
		n.Parent = parent
		n.Name = names[i]
		n.Depth = depth
	}
	return first
}

// setDirect - aplica os totais de arquivos diretos de um dir (chamado 1x pelo
// dono do dir). Sem lock: índice exclusivo; a visibilidade é garantida pelo
// WaitGroup antes de aggregate().
func (a *dirArena) setDirect(idx int32, size, alloc, files int64) {
	n := a.at(idx)
	n.Size += size
	n.Alloc += alloc
	n.Files += files
}

func (a *dirArena) depthOf(idx int32) uint16 { return a.at(idx).Depth }

// aggregate - soma bottom-up in-place. O(n), um passe.
func (a *dirArena) aggregate() {
	for i := a.len() - 1; i >= 1; i-- {
		n := a.at(int32(i))
		p := a.at(n.Parent)
		p.Size += n.Size
		p.Alloc += n.Alloc
		p.Files += n.Files
		p.Subdirs += n.Subdirs + 1
	}
}

// pathOf - caminho completo reconstruído pela cadeia de pais.
func (a *dirArena) pathOf(i int32) string {
	if i <= 0 || int(i) >= a.len() {
		if a.len() > 0 {
			return a.at(0).Name
		}
		return ""
	}
	var parts []string
	cur := i
	guard := 0
	for cur > 0 && guard < 256 {
		parts = append(parts, a.at(cur).Name)
		cur = a.at(cur).Parent
		guard++
	}
	base := strings.TrimSuffix(a.at(0).Name, `\`)
	var sb strings.Builder
	sb.WriteString(base)
	for j := len(parts) - 1; j >= 0; j-- {
		sb.WriteString(`\`)
		sb.WriteString(parts[j])
	}
	return sb.String()
}
