package main

// mft_par.go - pipeline PARALELO e sem alocacao para o motor MFT.
//
// Diagnostico que motivou este arquivo (medido; ver bench/):
//   o motor original fazia, numa UNICA goroutine, o ciclo
//        ler 8 MB  ->  parsear 8192 registros  ->  ler 8 MB  ->  ...
//   Ou seja: I/O e CPU NUNCA se sobrepunham, e 27 dos 28 nucleos ficavam
//   ociosos durante a varredura. Alem disso parseRecord fazia ate 4 varreduras
//   de atributos por registro e alocava no heap para CADA $FILE_NAME (inclusive
//   os descartados), gerando pressao de GC em 3M+ registros.
//
// Correcoes estruturais aqui:
//   1. I/O POSICIONADO (ReadFile com OVERLAPPED) -> N leituras concorrentes no
//      mesmo handle, em vez de um file pointer compartilhado serializado.
//   2. TAREFAS por faixa de indice de registro distribuídas por roubo de trabalho:
//      cada tarefa escreve em indices DISJUNTOS dos arrays compartilhados, logo
//      sem lock. A ordem dos registros e irrelevante para a arvore reconstruida.
//   3. UMA varredura de atributos por registro, decidindo $FILE_NAME e $DATA no
//      mesmo passo (o original fazia ate 4 varreduras).
//   4. Zero alocacao no caminho quente: o melhor nome vai para um scratch fixo
//      do shard; o nome e copiado para o nameBuf UMA unica vez.

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"sync"
)

const attrEndVal = 0xFFFFFFFF

// taskRecs - registros por tarefa = TAMANHO DE BLOCO default (16 MB com registros
// de 1 KB).
//
// Medido no volume cru do C: (selftest --iosweep, mesmo processo, mesmo regime):
//   4 MB -> 184,6 MB/s | 8 MB -> 199,5 | 16 MB -> 218,0 | 32 MB -> 218,5
// 16 MB entrega o mesmo que 32 MB e mantem 2 buffers no orcamento de 32 MB
// (com 32 MB o orcamento daria UM buffer so, serializando leitor e parser).
// O teto do instrumento independente no mesmo volume cru e 265-294 MB/s.
const taskRecs = 16384

// recSeg - trecho CONTIGUO do $MFT alinhado a recordSize.
type recSeg struct {
	startIdx int   // indice do primeiro registro
	count    int   // quantos registros
	phys     int64 // offset fisico (bytes) do inicio
}

// idxRange - faixa de indices que um shard escreveu (para corrigir nameOff).
type idxRange struct{ lo, hi int }

// task - unidade de trabalho: ler phys e parsear nrecs registros a partir de idx0.
type task struct {
	idx0  int
	nrecs int
	phys  int64
}

// shard - acumuladores privados de UM worker. Nada aqui e compartilhado.
type shard struct {
	nameBuf []byte
	scratch [520]byte // maior nome NTFS = 255 UTF-16 = 510 B
	owned   []idxRange

	extNames map[uint64][]extName
	extData  map[uint64]dataSeg
	contExt  map[int64][]mftRun

	inUse   int64
	unnamed int64
	reparse int64
	deleted int64
	extRecs int64
	parsed  int

	errN    int64
	errList []string
}

// buildRecSegs - achata a runlist em segmentos alinhados a recordSize.
// ok=false quando algum run nao esta alinhado (cai no caminho serial).
func (e *mftEngine) buildRecSegs(runs []mftRun) ([]recSeg, bool) {
	segs := make([]recSeg, 0, len(runs))
	rs := int64(e.recordSize)
	for _, r := range runs {
		start := r.vcn * e.clusterSize
		nbytes := r.clusters * e.clusterSize
		if start%rs != 0 || nbytes%rs != 0 {
			return nil, false
		}
		si := int(start / rs)
		if si >= e.recordCount {
			continue
		}
		cnt := int(nbytes / rs)
		if si+cnt > e.recordCount {
			cnt = e.recordCount - si
		}
		if cnt <= 0 {
			continue
		}
		segs = append(segs, recSeg{startIdx: si, count: cnt, phys: r.lcn * e.clusterSize})
	}
	if len(segs) == 0 {
		return nil, false
	}
	return segs, true
}

// pblock - bloco ja lido, aguardando parse no pipeline.
// off = deslocamento do 1o registro dentro do buffer (NO_BUFFERING le um
// superconjunto alinhado ao setor, entao o registro nao comeca no byte 0).
type pblock struct {
	buf   []byte
	off   int
	idx0  int
	nrecs int
}

// parseOne - parse de UM registro, sem alocacao no caminho comum.
// Escreve nos arrays compartilhados em idx (faixa exclusiva do worker).
func (e *mftEngine) parseOne(sh *shard, idx int, rec []byte) {
	sh.parsed++
	if rec[0] != 'F' || rec[1] != 'I' || rec[2] != 'L' || rec[3] != 'E' {
		sh.deleted++
		return
	}
	flags := uint16(rec[0x16]) | uint16(rec[0x17])<<8
	if flags&recInUse == 0 {
		sh.deleted++
		return
	}
	if idx != rootRecIdx && idx < 16 {
		sh.deleted++
		return
	}

	if baseRef := binary.LittleEndian.Uint64(rec[0x20:]) & 0xFFFFFFFFFFFF; baseRef != 0 {
		// ---- registro de EXTENSAO ----
		sh.extRecs++
		if e.fastMode {
			return
		}
		if !e.fixup(rec) {
			return
		}
		attrsOff := int(binary.LittleEndian.Uint16(rec[0x14:]))
		if names := extractNamesFrom(rec, attrsOff); len(names) > 0 {
			if sh.extNames == nil {
				sh.extNames = make(map[uint64][]extName, 64)
			}
			sh.extNames[baseRef] = append(sh.extNames[baseRef], names...)
		}
		if seg, ok := extractDataSeg(rec, attrsOff); ok {
			if sh.extData == nil {
				sh.extData = make(map[uint64]dataSeg, 64)
			}
			sh.extData[baseRef] = seg
		}
		return
	}

	if !e.fixup(rec) {
		sh.deleted++
		return
	}
	sh.inUse++

	isDir := flags&recDir != 0
	attrsOff := int(binary.LittleEndian.Uint16(rec[0x14:]))

	if e.needCont {
		for _, ex := range collectContExt(rec, attrsOff) {
			if sh.contExt == nil {
				sh.contExt = make(map[int64][]mftRun, 16)
			}
			sh.contExt[ex.lowVcn] = ex.runs
		}
	}

	// ---- UMA varredura de atributos ----
	bestRank := -1
	var bestParent uint64
	var bestSL, bestSA int64
	bestReparse := false
	scratchLen := 0

	nrAlloc, nrReal := int64(-1), int64(-1) // $DATA nao-residente (vcn 0)
	resLen := int64(-1)                     // $DATA residente

	for off := attrsOff; off+16 <= len(rec); {
		t := binary.LittleEndian.Uint32(rec[off:])
		if t == attrEndVal {
			break
		}
		alen := int(binary.LittleEndian.Uint32(rec[off+4:]))
		if alen < 16 || off+alen > len(rec) {
			break
		}
		switch {
		case t == 0x30 && rec[off+8] == 0 && rec[off+9] == 0:
			vlen := int(binary.LittleEndian.Uint32(rec[off+16:]))
			voff := int(binary.LittleEndian.Uint16(rec[off+20:]))
			if voff+vlen <= alen && vlen >= 66 {
				v := rec[off+voff : off+voff+vlen]
				if r := nsRank(v[65]); r > bestRank {
					bestRank = r
					bestParent = binary.LittleEndian.Uint64(v) & 0xFFFFFFFFFFFF
					bestSL = int64(binary.LittleEndian.Uint64(v[40:]))
					bestSA = int64(binary.LittleEndian.Uint64(v[48:]))
					bestReparse = binary.LittleEndian.Uint32(v[56:])&0x400 != 0
					nl := int(v[64])
					if 66+nl*2 <= vlen && nl*2 <= len(sh.scratch) {
						copy(sh.scratch[:], v[66:66+nl*2])
						scratchLen = nl * 2
					} else {
						scratchLen = 0
					}
				}
			}
		case t == 0x80 && rec[off+8] != 0 && rec[off+9] == 0:
			if nrReal < 0 && binary.LittleEndian.Uint64(rec[off+16:]) == 0 {
				nrAlloc = int64(binary.LittleEndian.Uint64(rec[off+40:]))
				nrReal = int64(binary.LittleEndian.Uint64(rec[off+48:]))
			}
		case t == 0x80 && rec[off+8] == 0 && rec[off+9] == 0:
			if resLen < 0 {
				resLen = int64(binary.LittleEndian.Uint32(rec[off+16:]))
			}
		}
		off += alen
	}

	// ---- precedencia IDENTICA ao motor original ----
	var sl, sa int64 = -1, -1
	hasData := false
	switch {
	case nrReal >= 0:
		sl, sa = nrReal, nrAlloc
		hasData = true
	case resLen >= 0:
		sl, sa = resLen, resLen
		hasData = true
	case bestRank >= 0:
		sl, sa = bestSL, bestSA
	}

	var f uint8
	if isDir {
		f |= recDir
	}
	if bestRank >= 0 && bestReparse {
		f |= recRepars
		sh.reparse++
	}
	if hasData {
		f |= recHasData
	}

	if bestRank < 0 {
		sh.unnamed++
		f |= recUnnamed
		e.flags[idx] = f
		return
	}

	f |= recNamed
	e.flags[idx] = f
	e.nameRank[idx] = uint8(bestRank + 1)
	e.nameOff[idx] = uint32(len(sh.nameBuf))
	sh.nameBuf = append(sh.nameBuf, sh.scratch[:scratchLen]...)
	e.nameLenB[idx] = uint16(scratchLen)
	e.parentU32[idx] = uint32(bestParent)
	if !isDir {
		if sl < 0 {
			sl = 0
		}
		if sa < 0 {
			sa = sl
		}
		e.sizeL[idx] = sl
		e.sizeA[idx] = sa
	}
}

// streamMFTPar - PIPELINE de varredura: 1 leitor SEQUENCIAL + N parsers.
//
// Por que leitor unico e sequencial: medido com bench/readprobe neste box,
// leitura sequencial bufferizada (264 MB/s em C:) e igual ou MAIS RAPIDA que 28
// threads lendo blocos espalhados (250 MB/s) - e em HDD leitura aleatoria
// paralela destroi a vazao (o cabecote passa a fazer seek). O ganho real vem de
// SOBREPOR esse I/O sequencial com o parse em N nucleos, nao de paralelizar o
// disco. `done` (startIdx do segmento -> registros ja parseados) permite
// chamadas sucessivas conforme extents de continuacao sao descobertos.
func (e *mftEngine) streamMFTPar(runs []mftRun, done map[int]int) int {
	segs, ok := e.buildRecSegs(runs)
	if !ok {
		return e.streamMFT(runs, 0) // runlist nao alinhada -> serial
	}

	// ---- monta a lista de blocos, em ORDEM FISICA (I/O sequencial) ----
	// O tamanho do bloco e ajustavel (--ioread) porque o otimo pode diferir
	// entre arquivo e volume cru; o default (4 MB) veio do readprobe.
	tr := e.readRecs
	if tr <= 0 {
		tr = taskRecs
	}
	var tasks []task
	skippedRecs := 0
	for _, sg := range segs {
		d := done[sg.startIdx]
		if d >= sg.count {
			continue
		}
		for k := d; k < sg.count; k += tr {
			n := sg.count - k
			if n > tr {
				n = tr
			}
			rec0 := sg.startIdx + k
			if e.bitmap != nil && !e.bitmap.chunkHasInUse(rec0, n) {
				skippedRecs += n
				continue
			}
			tasks = append(tasks, task{
				idx0:  rec0,
				nrecs: n,
				phys:  sg.phys + int64(k)*int64(e.recordSize),
			})
		}
		done[sg.startIdx] = sg.count
	}
	e.skippedRecs += skippedRecs
	e.deleted += int64(skippedRecs)

	if skippedRecs > 0 && os.Getenv("AETHERSCAN_TIMING") != "" {
		fmt.Fprintf(os.Stderr, "[mft-bitmap] pulados %d registros (~%.1f MB de I/O economizados)\n",
			skippedRecs, float64(skippedRecs*e.recordSize)/(1<<20))
	}
	if len(tasks) == 0 {
		return 0
	}

	workers := e.workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers < 1 {
		workers = 1
	}
	if workers > 64 {
		workers = 64
	}

	recSize := e.recordSize
	// +4096 de folga para o modo NO_BUFFERING (arredonda o ultimo bloco p/ cima)
	slack := 0
	if e.nobuf {
		slack = 4096
	}
	// ---- pool de buffers dimensionado por BYTES ----
	// Antes: workers+2 blocos. Com 28 workers e blocos de 4 MB isso e
	// 30 x 4 MB = 120 MB POR VOLUME — e com 4 volumes em paralelo passa de
	// 480 MB so de buffer, o que estoura a RAM do box (medido: 630 B/registro
	// de pico). O leitor e o gargalo (o parse e mais rapido que o disco), entao
	// poucos blocos ja mantem o pipeline cheio.
	bufBudget := 32 << 20
	if e.bufMB > 0 {
		bufBudget = e.bufMB << 20
	}
	blockBytes := tr*recSize + slack
	nbuf := bufBudget / blockBytes
	if nbuf < 3 {
		nbuf = 3
	}
	if nbuf > workers+2 {
		nbuf = workers + 2
	}
	if nbuf > len(tasks)+1 {
		nbuf = len(tasks) + 1
	}
	if nbuf < 1 {
		nbuf = 1
	}
	freeCh := make(chan []byte, nbuf)
	for i := 0; i < nbuf; i++ {
		freeCh <- e.allocBuf(blockBytes)
	}
	workCh := make(chan pblock, nbuf)

	shards := make([]*shard, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		sh := &shard{}
		shards[w] = sh
		go func(sh *shard) {
			defer wg.Done()
			for pb := range workCh {
				for k := 0; k < pb.nrecs; k++ {
					s := pb.off + k*recSize
					e.parseOne(sh, pb.idx0+k, pb.buf[s:s+recSize])
				}
				sh.owned = append(sh.owned, idxRange{pb.idx0, pb.idx0 + pb.nrecs})
				freeCh <- pb.buf
			}
		}(sh)
	}

	// ---- leitor sequencial: preenche o pipeline e roda no chamador ----
	for _, t := range tasks {
		buf := <-freeCh
		nb := t.nrecs * recSize

		if e.nobuf {
			// NO_BUFFERING exige offset e tamanho multiplos do setor fisico:
			// le o superconjunto alinhado e aponta o parse para o offset certo.
			a := t.phys &^ int64(sectorAlign-1)
			end := (t.phys + int64(nb) + int64(sectorAlign-1)) &^ int64(sectorAlign-1)
			rl := int(end - a)
			if rl > len(buf) {
				e.errN++
				if len(e.errList) < 200 {
					e.errList = append(e.errList, fmt.Sprintf("buffer curto p/ NO_BUFFERING @%d", a))
				}
				freeCh <- buf
				continue
			}
			if err := e.readFn(a, buf[:rl]); err != nil {
				e.errN++
				if len(e.errList) < 200 {
					e.errList = append(e.errList, fmt.Sprintf("ler MFT @%d: %v", a, err))
				}
				freeCh <- buf
				continue
			}
			workCh <- pblock{buf: buf, off: int(t.phys - a), idx0: t.idx0, nrecs: t.nrecs}
			continue
		}

		if err := e.readFn(t.phys, buf[:nb]); err != nil {
			e.errN++
			if len(e.errList) < 200 {
				e.errList = append(e.errList, fmt.Sprintf("ler MFT @%d: %v", t.phys, err))
			}
			freeCh <- buf
			continue
		}
		workCh <- pblock{buf: buf, off: 0, idx0: t.idx0, nrecs: t.nrecs}
	}
	close(workCh)
	wg.Wait()

	// ---- merge dos nameBufs (corrige offsets por faixa possuida) ----
	acc := uint32(len(e.nameBuf))
	bases := make([]uint32, workers)
	for w := range shards {
		bases[w] = acc
		acc += uint32(len(shards[w].nameBuf))
	}
	if acc > uint32(len(e.nameBuf)) {
		grown := make([]byte, acc)
		copy(grown, e.nameBuf)
		pos := uint32(len(e.nameBuf))
		for w := range shards {
			copy(grown[pos:], shards[w].nameBuf)
			pos += uint32(len(shards[w].nameBuf))
		}
		e.nameBuf = grown
	}
	for w := range shards {
		b := bases[w]
		if b == 0 {
			continue
		}
		for _, rg := range shards[w].owned {
			for i := rg.lo; i < rg.hi; i++ {
				if e.nameLenB[i] != 0 {
					e.nameOff[i] += b
				}
			}
		}
	}

	// ---- merge de contadores e mapas ----
	parsed := 0
	for _, sh := range shards {
		e.inUse += sh.inUse
		e.unnamed += sh.unnamed
		e.reparse += sh.reparse
		e.deleted += sh.deleted
		e.extRecs += sh.extRecs
		e.errN += sh.errN
		parsed += sh.parsed
		for k, v := range sh.extNames {
			e.extNames[k] = append(e.extNames[k], v...)
		}
		for k, v := range sh.extData {
			e.extData[k] = v
		}
		for k, v := range sh.contExt {
			e.contExt[k] = v
		}
		for _, msg := range sh.errList {
			if len(e.errList) >= 200 {
				break
			}
			e.errList = append(e.errList, msg)
		}
	}
	return parsed
}
