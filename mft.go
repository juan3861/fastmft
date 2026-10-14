package main

// mft.go - motor MFT: leitura DIRETA da Master File Table do NTFS.
//
// Esta é a peça "WizTree/Everything" (conceito) construída de forma aberta:
// em vez de varrer diretórios, lemos o índice mestre do NTFS (o próprio
// catálogo de arquivos do sistema) em blocos sequenciais e reconstruímos a
// árvore a partir das referências pai/filho dos registros.
//
// Referência de formato: repositório omerbenamram/mft (Rust) - offsets do
// atributo $FILE_NAME (0x30) conferidos byte a byte em src/attribute/x30.rs.
//
// Requer privilégio de administrador (acesso ao volume cru \\.\X:).

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// mftFileMode indica que a fonte é um arquivo (MFT cru ou imagem), não volume.

const (
	recInUse    = 0x01
	recDir      = 0x02
	recRepars   = 0x04
	recNamed    = 0x08
	recUnnamed  = 0x10 // em uso, sem nome no próprio registro (pode vir de extensão)
	recHasData  = 0x20 // $DATA (vcn 0) presente no próprio registro

	rootRecIdx = 5 // registro raiz do NTFS (".")
)

// mftRun - trecho contíguo de clusters. vcn é o VCN inicial ABSOLUTO (a partir
// do começo do arquivo), para que lacunas entre extents não desalinhem a
// contagem de registros.
type mftRun struct{ vcn, lcn, clusters int64 }

// offsetVCN - soma um VCN base a uma lista de runs (usado quando um atributo
// $DATA cobre VCNs a partir de lowestVcn > 0).
func offsetVCN(runs []mftRun, base int64) []mftRun {
	if base == 0 {
		return runs
	}
	for i := range runs {
		runs[i].vcn += base
	}
	return runs
}

type mftEngine struct {
	drive    string
	rootName string
	h        syscall.Handle

	// readFn - fonte de dados: volume cru (\\.\C:) ou arquivo (MFT cru / imagem).
	readFn func(off int64, buf []byte) error

	// rawMft - true quando a fonte é um dump CRU da MFT (sem boot sector).
	rawMft bool

	bytesPerSector int64
	clusterSize    int64
	recordSize     int

	mftStart int64

	recordCount int
	parentU32   []uint32
	sizeL       []int64
	sizeA       []int64
	flags       []uint8
	nameOff     []uint32
	nameLenB    []uint16
	nameBuf     []byte

	filesAgg   []int32
	subdirsAgg []int32
	childStart []int32
	childList  []int32

	inUse     int64
	unnamed   int64
	orphans   int64
	reparse   int64
	deleted   int64
	extRecs   int64
	metaBytes int64

	// diagnóstico do $MFT
	mftSegs     int // nº de ATRIBUTOS $DATA do $MFT (extents)
	mftRuns     int // nº de RUNS (trechos contíguos) da runlist completa
	mftDataRaw  int64
	mftRunBytes int64
	recsRead    int

	extMap map[extKey]*extStat

	// extNames - nomes ($FILE_NAME) encontrados em REGISTROS DE EXTENSÃO,
	// mapeados pelo registro base. Aplicados depois da varredura.
	extNames map[uint64][]extName
	// extData - segmento $DATA (vcn 0) achado em registro de extensão.
	extData map[uint64]dataSeg

	// needCont/contExt - descoberta de extents de continuação do $MFT.
	// needCont liga a coleta só quando a runlist inicial não cobre o $DATA
	// (MFT fragmentada) - custo zero no caso comum (MFT contígua).
	needCont bool
	contExt  map[int64][]mftRun

	// nameRank - rank do nome escolhido (nsRank+1; 0 = sem nome).
	nameRank []uint8

	dumpPat   string
	dumpPath  string
	dumpLines []string
	dumpMax   int

	// outDir - pasta de saída (para o dump de diagnóstico mft_runs.txt).
	outDir string
	// attrRaw/attrErr - bytes crus do runlist do $ATTRIBUTE_LIST do $MFT e o
	// erro (se houver) ao lê-lo.
	attrRaw []byte
	attrErr string

	started time.Time
	topN    int
	errN    int64
	errList []string

	includeMeta bool

	// ---- I/O ajustavel (F1) ----
	readRecs int  // registros por bloco de leitura (0 = default)
	bufMB    int  // orcamento do pool de buffers em MB (0 = 32 MB default)
	workers  int  // nº de parsers (0 = NumCPU). O leitor divide CPU com eles.
	nobuf    bool // FILE_FLAG_NO_BUFFERING + buffers alinhados a pagina

	// ---- snapshot (F4) ----
	// Gravado em build() ANTES da agregacao: aggregate() soma os tamanhos dos
	// filhos DENTRO de sizeL/sizeA, entao o estado pos-agregacao NAO e um estado
	// folha e re-agregar infla os valores (exponencial na profundidade). O
	// snapshot guarda sempre o estado FOLHA.
	snapDir     string
	snapUsnID   uint64
	snapNextUsn int64
}

// allocBuf - buffer de leitura; alinhado a pagina quando NO_BUFFERING.
func (e *mftEngine) allocBuf(n int) []byte {
	if e.nobuf {
		return alignedAlloc(n)
	}
	return make([]byte, n)
}

// dataSeg - segmento de $DATA de um registro de extensão.
type dataSeg struct {
	real, alloc int64
}

// extName - $FILE_NAME extraído de um registro (base ou extensão).
type extName struct {
	ns     byte
	nameB  []byte // UTF-16LE cru
	parent uint64
	sl, sa int64
}

func newMftEngine(drive string, topN int, includeMeta bool) *mftEngine {
	root := drive
	if len(root) == 2 {
		root = root + `\`
	}
	return &mftEngine{
		drive:       drive,
		rootName:    root,
		topN:        topN,
		includeMeta: includeMeta,
		extMap:      make(map[extKey]*extStat, 256),
		extNames:    make(map[uint64][]extName),
		extData:     make(map[uint64]dataSeg),
		contExt:     make(map[int64][]mftRun),
	}
}

func (e *mftEngine) recErr(msg string) {
	e.errN++
	if len(e.errList) < 200 {
		e.errList = append(e.errList, msg)
	}
}

// fixup - aplica a Update Sequence Array (correção de setores).
func (e *mftEngine) fixup(rec []byte) bool {
	usaOff := int(binary.LittleEndian.Uint16(rec[4:]))
	usaCnt := int(binary.LittleEndian.Uint16(rec[6:]))
	if usaCnt <= 1 {
		return true
	}
	if usaOff+usaCnt*2 > len(rec) {
		return false
	}
	stride := len(rec) / (usaCnt - 1)
	if stride < 2 || stride*(usaCnt-1) != len(rec) {
		return false
	}
	usn := binary.LittleEndian.Uint16(rec[usaOff:])
	for i := 1; i < usaCnt; i++ {
		pos := i*stride - 2
		if pos+2 > len(rec) {
			return false
		}
		if binary.LittleEndian.Uint16(rec[pos:]) != usn {
			return false
		}
		copy(rec[pos:pos+2], rec[usaOff+i*2:usaOff+i*2+2])
	}
	return true
}

func parseRunList(b []byte) []mftRun {
	var runs []mftRun
	var lcn, vcn int64
	for p := 0; p < len(b); {
		hdr := b[p]
		p++
		if hdr == 0 {
			break
		}
		lb := int(hdr & 0x0F)
		ob := int(hdr >> 4)
		if lb == 0 || p+lb+ob > len(b) {
			break
		}
		var length int64
		for i := lb - 1; i >= 0; i-- {
			length = length<<8 | int64(b[p+i])
		}
		p += lb
		var delta int64
		for i := ob - 1; i >= 0; i-- {
			delta = delta<<8 | int64(b[p+i])
		}
		p += ob
		if ob > 0 {
			shift := uint(64 - 8*ob)
			delta = delta << shift >> shift
			lcn += delta
			runs = append(runs, mftRun{vcn, lcn, length})
		}
		// runs esparsos (ob == 0) avançam o VCN mas não têm LCN
		vcn += length
	}
	return runs
}

func (e *mftEngine) open() error {
	h, err := openVolume(e.drive, e.nobuf)
	if err != nil {
		return fmt.Errorf("abrir volume %s (requer admin): %w", e.drive, err)
	}
	e.h = h
	e.readFn = func(off int64, buf []byte) error { return readVolumeAt(e.h, off, buf) }

	return e.parseBoot(func(boot []byte) error { return nil })
}

// readAligned - le n bytes a partir de off respeitando o alinhamento exigido por
// NO_BUFFERING: le o superconjunto alinhado ao setor e devolve a fatia pedida.
// No modo bufferizado e um read direto.
func (e *mftEngine) readAligned(off int64, n int) ([]byte, error) {
	if !e.nobuf {
		buf := make([]byte, n)
		if err := e.readFn(off, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	a := off &^ int64(sectorAlign-1)
	end := (off + int64(n) + int64(sectorAlign-1)) &^ int64(sectorAlign-1)
	raw := alignedAlloc(int(end - a))
	defer alignedFree(raw)
	if err := e.readFn(a, raw); err != nil {
		return nil, err
	}
	out := make([]byte, n)
	copy(out, raw[off-a:])
	return out, nil
}

// openFile - abre de um arquivo: MFT cru (começa com "FILE") ou imagem de disco
// NTFS (setor de boot válido). Útil para testes offline (sem admin).
func (e *mftEngine) openFile(path string) error {
	if e.nobuf {
		// NO_BUFFERING exige que TODA leitura caia dentro do arquivo: o
		// superconjunto alinhado ao setor do ultimo bloco pode passar do EOF e
		// o ReadAt devolveria io.EOF -> bloco descartado (mesma familia do bug
		// 6). Se o tamanho nao for multiplo do setor, cai para bufferizado.
		if st, err := os.Stat(path); err == nil && st.Size()%sectorAlign != 0 {
			fmt.Fprintf(os.Stderr, "aviso: --nobuf exige tamanho multiplo de %d bytes (arquivo tem %d); usando leitura bufferizada\n",
				sectorAlign, st.Size())
			e.nobuf = false
		}
	}
	if e.nobuf {
		h, err := openFile(path, genericRead, fileShareRead|fileShareWrite,
			fileFlagNoBuffering|fileFlagSequentialScan)
		if err != nil {
			return err
		}
		e.h = h
		e.readFn = func(off int64, buf []byte) error { return readVolumeAt(e.h, off, buf) }
	} else {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		e.h = syscall.Handle(f.Fd()) // mantido só p/ fechar; leitura via readFn
		e.readFn = func(off int64, buf []byte) error {
			_, err := f.ReadAt(buf, off)
			return err
		}
	}

	boot, err := e.readAligned(0, 512)
	if err != nil {
		return err
	}
	if string(boot[0:5]) == "FILE0" {
		// MFT cru: registros a partir do offset 0; recordSize padrão 1024
		e.rawMft = true
		e.bytesPerSector = 512
		e.clusterSize = 4096
		e.recordSize = 1024
		e.mftStart = 0
		if st, err := os.Stat(path); err == nil {
			e.recordCount = int(st.Size()) / e.recordSize
		}
		return nil
	}
	return e.parseBootFile()
}

// parseBoot - valida setor de boot e extrai geometria + localização da MFT.
func (e *mftEngine) parseBoot(_ func([]byte) error) error {
	boot, err := e.readAligned(0, 512)
	if err != nil {
		return fmt.Errorf("ler boot sector: %w", err)
	}
	if string(boot[3:11]) != "NTFS    " {
		return fmt.Errorf("assinatura NTFS ausente")
	}
	e.bytesPerSector = int64(binary.LittleEndian.Uint16(boot[0x0B:]))
	if e.bytesPerSector < 256 || e.bytesPerSector > 8192 {
		return fmt.Errorf("bytes/setor invalido: %d", e.bytesPerSector)
	}
	spc := int64(boot[0x0D])
	if spc == 0 {
		spc = 1
	}
	e.clusterSize = e.bytesPerSector * spc
	mftLcn := int64(binary.LittleEndian.Uint64(boot[0x30:]))
	e.mftStart = mftLcn * e.clusterSize

	cpf := int8(boot[0x40])
	if cpf > 0 {
		e.recordSize = int(int64(cpf) * e.clusterSize)
	} else {
		e.recordSize = 1 << uint(-cpf)
	}
	if e.recordSize < 128 || e.recordSize > 1<<16 {
		return fmt.Errorf("tamanho de registro MFT invalido: %d", e.recordSize)
	}
	return nil
}

func (e *mftEngine) parseBootFile() error { return e.parseBoot(nil) }

// mftExtent - um atributo $DATA (extent) do $MFT, com seu VCN inicial.
type mftExtent struct {
	lowVcn int64
	runs   []mftRun
}

// mftDataRuns - lê o registro 0 ($MFT) e monta a runlist COMPLETA do $DATA.
//
// Ponto crítico: quando o $DATA do $MFT é grande/fragmentado, o NTFS o divide
// em VÁRIOS atributos $DATA (extents), guardados em registros de EXTENSÃO
// referenciados pelo $ATTRIBUTE_LIST. Ler só o segmento 0 do registro 0
// truncava a MFT (pais ausentes -> filhos viravam órfãos). Aqui seguimos o
// $ATTRIBUTE_LIST e concatenamos todos os extents em ordem de VCN.
//
// Retorna (runs, dataSizeBytes, nExtents, err).
func (e *mftEngine) mftDataRuns() ([]mftRun, int64, int, error) {
	rec, err := e.readAligned(e.mftStart, e.recordSize)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("ler registro 0 da MFT: %w", err)
	}
	if string(rec[:4]) != "FILE" {
		return nil, 0, 0, fmt.Errorf("registro 0 da MFT invalido")
	}
	if !e.fixup(rec) {
		return nil, 0, 0, fmt.Errorf("fixup do registro 0 falhou")
	}

	type seg = mftExtent
	var segs []seg
	var dataSize int64 = -1
	var attrList []byte

	collect := func(buf []byte, wantList bool) {
		attrsOff := int(binary.LittleEndian.Uint16(buf[0x14:]))
		for off := attrsOff; off+16 <= len(buf); {
			t := binary.LittleEndian.Uint32(buf[off:])
			if t == 0xFFFFFFFF {
				break
			}
			l := int(binary.LittleEndian.Uint32(buf[off+4:]))
			if l < 16 || off+l > len(buf) {
				break
			}
			if t == 0x80 && buf[off+8] != 0 && buf[off+9] == 0 {
				lowVcn := int64(binary.LittleEndian.Uint64(buf[off+16:]))
				runOff := int(binary.LittleEndian.Uint16(buf[off+32:]))
				if off+runOff <= len(buf) {
					// VCN ABSOLUTO: essencial p/ o recOffset mapear refs altos.
					segs = append(segs, seg{lowVcn,
						offsetVCN(parseRunList(buf[off+runOff:off+l]), lowVcn)})
				}
				if lowVcn == 0 && dataSize < 0 {
					dataSize = int64(binary.LittleEndian.Uint64(buf[off+48:]))
				}
			}
			if wantList && t == 0x20 {
				if buf[off+8] == 0 {
					// $ATTRIBUTE_LIST residente
					vlen := int(binary.LittleEndian.Uint32(buf[off+16:]))
					voff := int(binary.LittleEndian.Uint16(buf[off+20:]))
					if vlen > 0 && voff+vlen <= l {
						attrList = append([]byte(nil), buf[off+voff:off+voff+vlen]...)
					}
				} else {
					// $ATTRIBUTE_LIST NÃO-RESIDENTE (caso do $MFT!): o próprio
					// catálogo guarda a lista. Lê-la via sua runlist.
					runOff := int(binary.LittleEndian.Uint16(buf[off+32:]))
					real := int64(binary.LittleEndian.Uint64(buf[off+48:]))
					if off+runOff <= len(buf) && real > 0 {
						e.attrRaw = append([]byte(nil), buf[off+runOff:off+l]...)
						rl := parseRunList(buf[off+runOff : off+l])
						if b, err := e.readRuns(rl, real); err == nil {
							attrList = b
						} else {
							e.attrErr = err.Error()
						}
					} else {
						e.attrErr = fmt.Sprintf("runOff=%d real=%d l=%d", runOff, real, l)
					}
				}
			}
			off += l
		}
	}

	collect(rec, true)
	if len(segs) == 0 || dataSize <= 0 {
		return nil, 0, 0, fmt.Errorf("MFT sem runlist $DATA (dataSize=%d)", dataSize)
	}

	// Segue $ATTRIBUTE_LIST -> extents $DATA em registros de extensão.
	// Entrada: type(4) len(2) nameLen(1) nameOff(1) lowestVcn(8) ref(8) attrId(2)
	if len(attrList) > 0 {
		for off := 0; off+26 <= len(attrList); {
			t := binary.LittleEndian.Uint32(attrList[off:])
			el := int(binary.LittleEndian.Uint16(attrList[off+4:]))
			if el < 26 || off+el > len(attrList) {
				break
			}
			lowVcn := int64(binary.LittleEndian.Uint64(attrList[off+8:]))
			ref := binary.LittleEndian.Uint64(attrList[off+16:]) & 0xFFFFFFFFFFFF
			if t == 0x80 && lowVcn > 0 && ref > 0 {
				if bo, ok := e.recOffset(segs, ref); ok {
					if b2, err := e.readAligned(bo, e.recordSize); err == nil &&
						string(b2[:4]) == "FILE" && e.fixup(b2) {
						collect(b2, false)
					}
				}
			}
			off += el
		}
	}

	sort.Slice(segs, func(i, j int) bool { return segs[i].lowVcn < segs[j].lowVcn })

	// ---- fallback robusto: extents de continuação ----
	// Se a runlist ainda não cobre todo o $DATA (ex.: $ATTRIBUTE_LIST
	// inacessível), procura extents $DATA com lowVcn>0 varrendo os primeiros
	// registros da MFT (os registros de extensão do $MFT são metadados
	// alocados cedo no catálogo).
	for round := 0; round < 8; round++ {
		if segContiguousVCN(segs)*e.clusterSize >= dataSize {
			break
		}
		extra := e.findContinuationExtents(segs, dataSize/int64(e.recordSize))
		if len(extra) == 0 {
			break
		}
		seen := make(map[int64]bool, len(segs))
		for _, s := range segs {
			seen[s.lowVcn] = true
		}
		added := 0
		for _, s := range extra {
			if !seen[s.lowVcn] {
				segs = append(segs, s)
				seen[s.lowVcn] = true
				added++
			}
		}
		if added == 0 {
			break
		}
		sort.Slice(segs, func(i, j int) bool { return segs[i].lowVcn < segs[j].lowVcn })
	}

	if e.outDir != "" || e.dumpPath != "" {
		e.writeAttrListDump(attrList, segs, dataSize)
	}

	sort.Slice(segs, func(i, j int) bool { return segs[i].lowVcn < segs[j].lowVcn })
	var runs []mftRun
	for _, s := range segs {
		runs = append(runs, s.runs...) // VCN já é absoluto
	}
	return runs, dataSize, len(segs), nil
}

// segContiguousVCN - VCN coberto de forma CONTÍGUA a partir de 0 pelos
// extents conhecidos (para de contar no primeiro buraco).
func segContiguousVCN(segs []mftExtent) int64 {
	sorted := append([]mftExtent(nil), segs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].lowVcn < sorted[j].lowVcn })
	var vcn int64
	for _, s := range sorted {
		if s.lowVcn > vcn {
			break
		}
		end := s.lowVcn
		for _, r := range s.runs {
			end += r.clusters
		}
		if end > vcn {
			vcn = end
		}
	}
	return vcn
}

// findContinuationExtents - varre os primeiros registros da MFT procurando
// atributos $DATA com lowestVcn > 0 (extents de continuação do $MFT).
func (e *mftEngine) findContinuationExtents(segs []mftExtent, maxRecs int64) []mftExtent {
	limit := int64(1) << 18
	if maxRecs < limit {
		limit = maxRecs
	}
	var extra []mftExtent
	cp := make([]byte, e.recordSize)
	for ridx := int64(1); ridx < limit; ridx++ {
		bo, ok := e.recOffset(segs, uint64(ridx))
		if !ok {
			break
		}
		rr, err := e.readAligned(bo, e.recordSize)
		if err != nil {
			continue
		}
		if string(rr[:4]) != "FILE" {
			continue
		}
		copy(cp, rr)
		if !e.fixup(cp) {
			continue
		}
		if binary.LittleEndian.Uint64(cp[0x20:])&0xFFFFFFFFFFFF != 0 {
			continue // registro de extensão comum -> base != 0
		}
		attrsOff := int(binary.LittleEndian.Uint16(cp[0x14:]))
		for off := attrsOff; off+16 <= len(cp); {
			t := binary.LittleEndian.Uint32(cp[off:])
			if t == 0xFFFFFFFF {
				break
			}
			l := int(binary.LittleEndian.Uint32(cp[off+4:]))
			if l < 16 || off+l > len(cp) {
				break
			}
			if t == 0x80 && cp[off+8] != 0 && cp[off+9] == 0 {
				lowVcn := int64(binary.LittleEndian.Uint64(cp[off+16:]))
				if lowVcn > 0 {
					runOff := int(binary.LittleEndian.Uint16(cp[off+32:]))
					if off+runOff <= len(cp) {
						extra = append(extra, mftExtent{lowVcn,
							offsetVCN(parseRunList(cp[off+runOff:off+l]), lowVcn)})
					}
				}
			}
			off += l
		}
	}
	return extra
}

// writeAttrListDump - grava o estado da runlist do $MFT (diagnóstico).
func (e *mftEngine) writeAttrListDump(attrList []byte, segs []mftExtent, dataSize int64) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "attrList bytes=%d | dataSize=%d | extents=%d | cluster=%d | recordSize=%d\n",
		len(attrList), dataSize, len(segs), e.clusterSize, e.recordSize)
	if e.attrErr != "" {
		fmt.Fprintf(&sb, "attrList ERRO: %s\n", e.attrErr)
	}
	if len(e.attrRaw) > 0 {
		fmt.Fprintf(&sb, "attrList runlist raw (%d bytes): % X\n", len(e.attrRaw), e.attrRaw)
	}
	for off := 0; off+26 <= len(attrList); {
		t := binary.LittleEndian.Uint32(attrList[off:])
		el := int(binary.LittleEndian.Uint16(attrList[off+4:]))
		if el < 26 || off+el > len(attrList) {
			fmt.Fprintf(&sb, "  BAD entry off=%d len=%d\n", off, el)
			break
		}
		lowVcn := int64(binary.LittleEndian.Uint64(attrList[off+8:]))
		ref := binary.LittleEndian.Uint64(attrList[off+16:]) & 0xFFFFFFFFFFFF
		aid := binary.LittleEndian.Uint16(attrList[off+24:])
		fmt.Fprintf(&sb, "  ENTRY off=%d type=0x%X len=%d lowVcn=%d ref=%d attrId=%d\n", off, t, el, lowVcn, ref, aid)
		off += el
	}
	var have int64
	nruns := 0
	for _, s := range segs {
		var c int64
		vcn := s.lowVcn
		for _, r := range s.runs {
			fmt.Fprintf(&sb, "  RUN  vcn=%d clusters=%d lcn=%d (off=%.1f MB, tam=%.2f MB)\n",
				vcn, r.clusters, r.lcn, float64(r.lcn*e.clusterSize)/(1<<20),
				float64(r.clusters*e.clusterSize)/(1<<20))
			vcn += r.clusters
			c += r.clusters
			nruns++
		}
		have += c
		fmt.Fprintf(&sb, "  seg lowVcn=%d clusters=%d lcn0=%d\n", s.lowVcn, c, func() int64 {
			if len(s.runs) > 0 {
				return s.runs[0].lcn
			}
			return -1
		}())
	}
	fmt.Fprintf(&sb, "runs totais=%d\n", nruns)
	fmt.Fprintf(&sb, "soma=%d bytes | contiguo=%d bytes | faltando(soma)=%d bytes\n",
		have*e.clusterSize, segContiguousVCN(segs)*e.clusterSize, dataSize-have*e.clusterSize)
	if e.outDir != "" {
		os.WriteFile(filepath.Join(e.outDir, "mft_runs.txt"), []byte(sb.String()), 0o666)
	}
}

// readRuns - lê n bytes a partir de uma runlist (usado p/ o $ATTRIBUTE_LIST
// não-residente do $MFT, que vive dentro do próprio catálogo).
//
// IMPORTANTE: leitura de volume cru exige tamanho MÚLTIPLO DO SETOR; pedir
// exatamente 160 bytes devolvia ERROR_INVALID_PARAMETER.
func (e *mftEngine) readRuns(runs []mftRun, n int64) ([]byte, error) {
	if n <= 0 {
		return nil, fmt.Errorf("tamanho invalido: %d", n)
	}
	const align = 4096
	aligned := (n + align - 1) / align * align
	out := make([]byte, 0, aligned)
	for _, r := range runs {
		if int64(len(out)) >= aligned {
			break
		}
		rb := r.clusters * e.clusterSize
		want := aligned - int64(len(out))
		if want > rb {
			want = rb
		}
		tmp, err := e.readAligned(r.lcn*e.clusterSize, int(want))
		if err != nil {
			return nil, err
		}
		out = append(out, tmp...)
	}
	if int64(len(out)) < n {
		return out, fmt.Errorf("runlist curta: %d < %d", len(out), n)
	}
	return out[:n], nil
}

// recOffset - mapeia o índice de um registro para offset físico, usando os
// VCNs absolutos das runs conhecidas (tolera lacunas).
func (e *mftEngine) recOffset(segs []mftExtent, recIdx uint64) (int64, bool) {
	target := int64(recIdx) * int64(e.recordSize)
	for _, s := range segs {
		for _, r := range s.runs {
			start := r.vcn * e.clusterSize
			end := (r.vcn + r.clusters) * e.clusterSize
			if target >= start && target < end {
				return r.lcn*e.clusterSize + (target - start), true
			}
		}
	}
	return 0, false
}

// runsContiguousVCN - VCN coberto de forma contígua a partir de 0 pelas runs.
func runsContiguousVCN(runs []mftRun) int64 {
	sorted := append([]mftRun(nil), runs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].vcn < sorted[j].vcn })
	var vcn int64
	for _, r := range sorted {
		if r.vcn > vcn {
			break
		}
		if end := r.vcn + r.clusters; end > vcn {
			vcn = end
		}
	}
	return vcn
}

// extractNamesFrom - varre os atributos $30 de um registro e devolve os nomes.
func extractNamesFrom(rec []byte, attrsOff int) []extName {
	var out []extName
	for off := attrsOff; off+16 <= len(rec); {
		t := binary.LittleEndian.Uint32(rec[off:])
		if t == 0xFFFFFFFF {
			break
		}
		alen := int(binary.LittleEndian.Uint32(rec[off+4:]))
		if alen < 16 || off+alen > len(rec) {
			break
		}
		if t == 0x30 && rec[off+8] == 0 && rec[off+9] == 0 {
			vlen := int(binary.LittleEndian.Uint32(rec[off+16:]))
			voff := int(binary.LittleEndian.Uint16(rec[off+20:]))
			if voff+vlen <= alen && vlen >= 66 {
				v := rec[off+voff : off+voff+vlen]
				nl := int(v[64])
				if 66+nl*2 <= len(v) {
					out = append(out, extName{
						ns:     v[65],
						nameB:  append([]byte(nil), v[66:66+nl*2]...),
						parent: binary.LittleEndian.Uint64(v) & 0xFFFFFFFFFFFF,
						sl:     int64(binary.LittleEndian.Uint64(v[40:])),
						sa:     int64(binary.LittleEndian.Uint64(v[48:])),
					})
				}
			}
		}
		off += alen
	}
	return out
}

// extractDataSeg - procura $DATA não-residente com lowest_vcn == 0 e devolve
// os tamanhos totais (o segmento vcn 0 carrega allocated/real do arquivo).
func extractDataSeg(rec []byte, attrsOff int) (dataSeg, bool) {
	for off := attrsOff; off+16 <= len(rec); {
		t := binary.LittleEndian.Uint32(rec[off:])
		if t == 0xFFFFFFFF {
			break
		}
		alen := int(binary.LittleEndian.Uint32(rec[off+4:]))
		if alen < 16 || off+alen > len(rec) {
			break
		}
		if t == 0x80 && rec[off+8] != 0 && rec[off+9] == 0 {
			lowVcn := binary.LittleEndian.Uint64(rec[off+16:])
			if lowVcn == 0 {
				return dataSeg{
					alloc: int64(binary.LittleEndian.Uint64(rec[off+40:])),
					real:  int64(binary.LittleEndian.Uint64(rec[off+48:])),
				}, true
			}
		}
		off += alen
	}
	return dataSeg{}, false
}

func nsRank(ns byte) int {
	switch ns {
	case 3: // Win32AndDos
		return 3
	case 1: // Win32
		return 2
	case 0: // POSIX
		return 1
	default: // DOS
		return 0
	}
}

// maybeDump - modo de diagnóstico: se dumpPat != "", e o registro contém algum
// dos padrões (separados por '|') nos bytes crus (UTF-16LE), grava um dump
// completo dos atributos. Funciona mesmo para registros deletados/sem-nome.
func (e *mftEngine) maybeDump(idx int, rec []byte, kind string) {
	if e.dumpPat == "" || len(e.dumpLines) >= e.dumpMax {
		return
	}
	needles := strings.Split(e.dumpPat, "|")
	hit := false
	for _, nd := range needles {
		if nd != "" && containsUTF16LE(rec, nd) {
			hit = true
			break
		}
	}
	if !hit {
		return
	}

	cp := make([]byte, len(rec))
	copy(cp, rec)
	_ = e.fixup(cp)
	attrsOff := int(binary.LittleEndian.Uint16(cp[0x14:]))
	names := extractNamesFrom(cp, attrsOff)

	var b strings.Builder
	flags := uint16(cp[0x16]) | uint16(cp[0x17])<<8
	baseRef := binary.LittleEndian.Uint64(cp[0x20:]) & 0xFFFFFFFFFFFF
	fmt.Fprintf(&b, "=== %s idx=%d flags=0x%04X inUse=%v isDir=%v baseRef=%d linkCount=%d\n",
		kind, idx, flags, flags&recInUse != 0, flags&recDir != 0, baseRef,
		binary.LittleEndian.Uint16(cp[0x12:]))
	for _, nm := range names {
		fmt.Fprintf(&b, "    NAME ns=%d parent=%d sl=%d sa=%d %q\n",
			nm.ns, nm.parent, nm.sl, nm.sa, utf16Bytes(nm.nameB))
	}
	for off := attrsOff; off+16 <= len(cp); {
		t := binary.LittleEndian.Uint32(cp[off:])
		if t == 0xFFFFFFFF {
			b.WriteString("    ATTR end\n")
			break
		}
		alen := int(binary.LittleEndian.Uint32(cp[off+4:]))
		if alen < 16 || off+alen > len(cp) {
			fmt.Fprintf(&b, "    ATTR BAD len=%d off=%d\n", alen, off)
			break
		}
		nonRes := cp[off+8] != 0
		nameLen := cp[off+9]
		if nonRes {
			lowVcn := binary.LittleEndian.Uint64(cp[off+16:])
			lastVcn := binary.LittleEndian.Uint64(cp[off+24:])
			runOff := binary.LittleEndian.Uint16(cp[off+32:])
			alloc := binary.LittleEndian.Uint64(cp[off+40:])
			real := binary.LittleEndian.Uint64(cp[off+48:])
			fmt.Fprintf(&b, "    ATTR type=0x%X len=%d nonRes=1 nameLen=%d lowVcn=%d lastVcn=%d runOff=%d alloc=%d real=%d\n",
				t, alen, nameLen, lowVcn, lastVcn, runOff, alloc, real)
		} else {
			vlen := binary.LittleEndian.Uint32(cp[off+16:])
			voff := binary.LittleEndian.Uint16(cp[off+20:])
			fmt.Fprintf(&b, "    ATTR type=0x%X len=%d nonRes=0 nameLen=%d vlen=%d voff=%d\n",
				t, alen, nameLen, vlen, voff)
		}
		off += alen
	}
	e.dumpLines = append(e.dumpLines, b.String())
}

// containsUTF16LE - busca "needle" (ASCII) codificado em UTF-16LE nos bytes
// crus de um registro, testando a forma original e as caixas baixa/alta.
func containsUTF16LE(hay []byte, needle string) bool {
	n := len(needle)
	if n == 0 || len(hay) < n*2 {
		return false
	}
	low := make([]byte, n*2)
	up := make([]byte, n*2)
	for i := 0; i < n; i++ {
		c := needle[i]
		lc := c
		if lc >= 'A' && lc <= 'Z' {
			lc += 32
		}
		uc := c
		if uc >= 'a' && uc <= 'z' {
			uc -= 32
		}
		low[i*2] = lc
		up[i*2] = uc
	}
	return bytes.Contains(hay, low) || bytes.Contains(hay, up)
}

// writeDump - grava as linhas de diagnóstico acumuladas.
func (e *mftEngine) writeDump() {
	if e.dumpPath == "" || len(e.dumpLines) == 0 {
		return
	}
	os.WriteFile(e.dumpPath, []byte(strings.Join(e.dumpLines, "\n")), 0o666)
}

// parseRecord - extrai nome, pai e tamanhos de um registro FILE.
func (e *mftEngine) parseRecord(idx int, rec []byte) {
	if rec[0] != 'F' || rec[1] != 'I' || rec[2] != 'L' || rec[3] != 'E' {
		e.deleted++
		return
	}
	if e.dumpPat != "" {
		kind := "BASE"
		if binary.LittleEndian.Uint64(rec[0x20:])&0xFFFFFFFFFFFF != 0 {
			kind = "EXT"
		}
		e.maybeDump(idx, rec, kind)
	}
	flags := uint16(rec[0x16]) | uint16(rec[0x17])<<8
	if flags&recInUse == 0 {
		e.deleted++
		return
	}
	if idx != rootRecIdx && idx < 16 {
		// metadados NTFS ($MFT, $LogFile, ...) - fora das estatisticas
		e.deleted++
		return
	}
	if baseRef := binary.LittleEndian.Uint64(rec[0x20:]) & 0xFFFFFFFFFFFF; baseRef != 0 {
		// Registro de EXTENSÃO: pode conter $FILE_NAME adicionais (hardlink ou
		// namespace alternativo) e trechos do $DATA. Guardamos para aplicar
		// no registro base depois da varredura (o base vem antes, mas por
		// garantia não assumimos ordem).
		e.extRecs++
		if !e.fixup(rec) {
			return
		}
		attrsOff := int(binary.LittleEndian.Uint16(rec[0x14:]))
		names := extractNamesFrom(rec, attrsOff)
		if len(names) > 0 {
			e.extNames[baseRef] = append(e.extNames[baseRef], names...)
		}
		if seg, ok := extractDataSeg(rec, attrsOff); ok {
			e.extData[baseRef] = seg
		}
		return
	}
	if !e.fixup(rec) {
		e.deleted++
		return
	}
	e.inUse++

	isDir := flags&recDir != 0
	attrsOff := int(binary.LittleEndian.Uint16(rec[0x14:]))

	// Registros com baseRef == 0 que contenham $DATA com lowestVcn > 0 são os
	// EXTENTS DE CONTINUAÇÃO do $MFT (o $MFT tem número de registro 0, logo
	// seus registros de extensão também têm baseRef == 0). Coletamos para
	// estender a runlist - só quando a MFT está fragmentada (needCont).
	if e.needCont {
		for _, ex := range collectContExt(rec, attrsOff) {
			e.contExt[ex.lowVcn] = ex.runs
		}
	}

	// coleta nomes via helper (mesma lógica usada p/ extensões)
	names := extractNamesFrom(rec, attrsOff)
	var bestRank = -1
	var best extName
	for _, nm := range names {
		r := nsRank(nm.ns)
		if r > bestRank {
			bestRank = r
			best = nm
		}
	}

	// $DATA (vcn 0) do próprio registro
	var sl, sa int64 = -1, -1
	hasData := false
	if seg, ok := extractDataSeg(rec, attrsOff); ok {
		sl, sa = seg.real, seg.alloc
		hasData = true
	}
	// $DATA residente (arquivos pequenos) tem prioridade se não há não-residente
	if !hasData {
		for off := attrsOff; off+16 <= len(rec); {
			t := binary.LittleEndian.Uint32(rec[off:])
			if t == 0xFFFFFFFF {
				break
			}
			alen := int(binary.LittleEndian.Uint32(rec[off+4:]))
			if alen < 16 || off+alen > len(rec) {
				break
			}
			if t == 0x80 && rec[off+8] == 0 && rec[off+9] == 0 {
				vl := int64(binary.LittleEndian.Uint32(rec[off+16:]))
				sl, sa = vl, vl
				hasData = true
				break
			}
			off += alen
		}
	}
	// fallback: tamanhos do $FILE_NAME escolhido
	if !hasData && bestRank >= 0 {
		sl, sa = best.sl, best.sa
	}

	var rep bool
	if bestRank >= 0 {
		// reparse flag: relê do $FILE_NAME escolhido (offset 56 do valor)
		for off := attrsOff; off+16 <= len(rec); {
			t := binary.LittleEndian.Uint32(rec[off:])
			if t == 0xFFFFFFFF {
				break
			}
			alen := int(binary.LittleEndian.Uint32(rec[off+4:]))
			if alen < 16 || off+alen > len(rec) {
				break
			}
			if t == 0x30 && rec[off+8] == 0 && rec[off+9] == 0 {
				vlen := int(binary.LittleEndian.Uint32(rec[off+16:]))
				voff := int(binary.LittleEndian.Uint16(rec[off+20:]))
				if voff+vlen <= alen && vlen >= 60 {
					v := rec[off+voff : off+voff+vlen]
					if byte(v[65]) == best.ns {
						fl := binary.LittleEndian.Uint32(v[56:])
						if fl&0x400 != 0 {
							rep = true
							break
						}
					}
				}
			}
			off += alen
		}
	}

	// flags básicas (mesmo sem nome - pode ser resolvido por extensão depois)
	f := uint8(0)
	if isDir {
		f |= recDir
	}
	if rep {
		f |= recRepars
		e.reparse++
	}
	if hasData {
		f |= recHasData
	}

	if bestRank < 0 {
		e.unnamed++
		f |= recUnnamed
		e.flags[idx] = f
		return
	}

	f |= recNamed
	e.flags[idx] = f
	e.nameRank[idx] = uint8(bestRank + 1)
	e.nameOff[idx] = uint32(len(e.nameBuf))
	e.nameBuf = append(e.nameBuf, best.nameB...)
	e.nameLenB[idx] = uint16(len(best.nameB))
	e.parentU32[idx] = uint32(best.parent)
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

// applyExtensions - aplica nomes e $DATA achados em registros de extensão.
// Regras:
//   - nome de rank maior substitui o do registro base;
//   - registro "unnamed" ganha nome se a extensão tiver um;
//   - $DATA vcn-0 da extensão preenche tamanho quando o base não tem $DATA.
func (e *mftEngine) applyExtensions() {
	// Visita a UNIÃO das chaves: um arquivo pode ter $DATA em extensão sem ter
	// nenhum $FILE_NAME extra lá (caso clássico de arquivo grande com um só
	// nome) - iterar só sobre extNames perderia esses $DATA.
	keys := make(map[uint64]struct{}, len(e.extNames)+len(e.extData))
	for k := range e.extNames {
		keys[k] = struct{}{}
	}
	for k := range e.extData {
		keys[k] = struct{}{}
	}
	for baseRef := range keys {
		names := e.extNames[baseRef]
		idx := int(baseRef)
		if idx <= 0 || idx >= e.recordCount {
			continue
		}
		f := e.flags[idx]
		if f&(recNamed|recUnnamed) == 0 {
			continue // registro não está em uso como base
		}
		bestRank := int(e.nameRank[idx])
		var best *extName
		for i := range names {
			r := nsRank(names[i].ns) + 1
			if r > bestRank {
				bestRank = r
				best = &names[i]
			}
		}
		changed := false
		if best != nil {
			// upgrade de nome
			f &^= recUnnamed
			f |= recNamed
			e.flags[idx] = f
			e.nameRank[idx] = uint8(bestRank)
			e.nameOff[idx] = uint32(len(e.nameBuf))
			e.nameBuf = append(e.nameBuf, best.nameB...)
			e.nameLenB[idx] = uint16(len(best.nameB))
			e.parentU32[idx] = uint32(best.parent)
			changed = true
		}
		// $DATA da extensão
		if seg, ok := e.extData[baseRef]; ok && e.flags[idx]&recHasData == 0 && !e.isDirFlag(idx) {
			e.sizeL[idx] = seg.real
			e.sizeA[idx] = seg.alloc
			e.flags[idx] |= recHasData
			changed = true
		}
		if changed && best == nil && e.flags[idx]&recHasData == 0 {
			// tamanho via $FILE_NAME se ainda faltar
			if e.sizeL[idx] == 0 && e.sizeA[idx] == 0 {
			}
		}
	}
	// zera contador de "unnamed" que ganhou nome
	// (recontagem simples)
	e.unnamed = 0
	for i := 1; i < e.recordCount; i++ {
		if e.flags[i]&recUnnamed != 0 {
			e.unnamed++
		}
	}
}

func (e *mftEngine) isDirFlag(idx int) bool {
	return e.flags[idx]&recDir != 0
}

// build - lê a MFT inteira e reconstrói a árvore.
func (e *mftEngine) build() error {
	var runs []mftRun
	var dataSize int64

	if e.rawMft {
		// MFT cru (dump): os registros são sequenciais do offset 0
		dataSize = int64(e.recordCount) * int64(e.recordSize)
		totalClusters := (dataSize + e.clusterSize - 1) / e.clusterSize
		runs = []mftRun{{0, 0, totalClusters}}
		e.mftSegs = 1
		e.mftRuns = 1
		e.mftDataRaw = dataSize
	} else {
		var err error
		var nseg int
		runs, dataSize, nseg, err = e.mftDataRuns()
		if err != nil {
			return err
		}
		e.mftSegs = nseg
		e.mftRuns = len(runs)
		e.mftDataRaw = dataSize
		var totalRunBytes int64
		for _, r := range runs {
			totalRunBytes += r.clusters * e.clusterSize
		}
		e.mftRunBytes = totalRunBytes
		// recordCount = tamanho REAL do $MFT (a runlist inicial pode cobrir
		// menos; extents de continuação são descobertos durante a leitura).
		e.recordCount = int(dataSize / int64(e.recordSize))
	}
	if e.recordCount < 16 {
		return fmt.Errorf("MFT pequena demais (%d registros)", e.recordCount)
	}

	n := e.recordCount
	e.parentU32 = make([]uint32, n)
	e.sizeL = make([]int64, n)
	e.sizeA = make([]int64, n)
	e.flags = make([]uint8, n)
	e.nameOff = make([]uint32, n)
	e.nameRank = make([]uint8, n)
	e.nameLenB = make([]uint16, n)
	e.nameBuf = make([]byte, 0, n*16)
	e.filesAgg = make([]int32, n)
	e.subdirsAgg = make([]int32, n)

	// AETHERSCAN_TIMING=1 imprime o custo de cada fase (diagnostico de gargalo).
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

	// ---- streaming da MFT (com descoberta de extents de continuação) ----
	e.contExt = make(map[int64][]mftRun)
	e.needCont = runsContiguousVCN(runs)*e.clusterSize < dataSize

	// O modo diagnostico (--find) dumpa registro a registro -> caminho serial.
	// Caso normal: pipeline paralelo (mft_par.go).
	// AETHERSCAN_SERIAL=1 forca o caminho serial ORIGINAL (mesmo binario, mesma
	// I/O): e assim que o A/B antes/depois fica perfeitamente controlado.
	// NO_BUFFERING exige o pipeline (o caminho serial le blocos nao alinhados).
	parallel := e.nobuf || (e.dumpPat == "" && os.Getenv("AETHERSCAN_SERIAL") == "")
	done := make(map[int]int)
	parsed := 0
	pass := func() {
		if parallel {
			parsed += e.streamMFTPar(runs, done)
		} else {
			parsed += e.streamMFT(runs, parsed)
		}
	}
	pass()
	extents := e.mftSegs
	for round := 0; round < 24; round++ {
		if runsContiguousVCN(runs)*e.clusterSize >= dataSize {
			break
		}
		newRuns, added := e.extendRuns(runs, dataSize)
		if added == 0 {
			break
		}
		runs = newRuns
		extents += added
		pass()
	}
	e.mftSegs = extents
	{
		var cov int64
		for _, r := range runs {
			cov += r.clusters
		}
		e.mftRunBytes = cov * e.clusterSize
	}
	mark("stream_mft")

	// ---- aplicar nomes/$DATA de registros de extensão ----
	e.recsRead = parsed
	e.applyExtensions()
	mark("extensions")

	if e.snapDir != "" {
		if err := e.SaveSnapshot(e.snapDir, nil, e.snapUsnID, e.snapNextUsn); err != nil {
			fmt.Fprintf(os.Stderr, "[mft] aviso: snapshot: %v\n", err)
		}
	}
	if err := e.aggregate(); err != nil {
		return err
	}
	return nil
}

// collectContExt - extrai extents $DATA com lowestVcn > 0 de um registro.
func collectContExt(rec []byte, attrsOff int) []mftExtent {
	var out []mftExtent
	for off := attrsOff; off+16 <= len(rec); {
		t := binary.LittleEndian.Uint32(rec[off:])
		if t == 0xFFFFFFFF {
			break
		}
		alen := int(binary.LittleEndian.Uint32(rec[off+4:]))
		if alen < 16 || off+alen > len(rec) {
			break
		}
		if t == 0x80 && rec[off+8] != 0 && rec[off+9] == 0 {
			lowVcn := int64(binary.LittleEndian.Uint64(rec[off+16:]))
			if lowVcn > 0 {
				runOff := int(binary.LittleEndian.Uint16(rec[off+32:]))
				if off+runOff <= len(rec) {
					out = append(out, mftExtent{lowVcn,
						offsetVCN(parseRunList(rec[off+runOff:off+alen]), lowVcn)})
				}
			}
		}
		off += alen
	}
	return out
}

// streamMFT - lê a MFT seguindo a runlist, parseando registros com índice
// >= startIdx. Retorna quantos registros foram efetivamente parseados.
func (e *mftEngine) streamMFT(runs []mftRun, startIdx int) int {
	buf := make([]byte, 8<<20)
	pending := make([]byte, 0, e.recordSize)
	parsed := 0
	lastTick := time.Now()
	for _, run := range runs {
		recStart := int(run.vcn * e.clusterSize / int64(e.recordSize))
		if recStart >= e.recordCount {
			continue
		}
		runRecords := int(run.clusters * e.clusterSize / int64(e.recordSize))
		if recStart+runRecords <= startIdx {
			continue
		}
		runBytes := run.clusters * e.clusterSize
		// NUNCA ler alem do fim real do $MFT. O nº de clusters e arredondado
		// PARA CIMA, entao runBytes pode exceder o tamanho do $DATA em ate
		// clusterSize-1 bytes. A ultima leitura entao ultrapassa o EOF, o
		// os.File.ReadAt devolve io.EOF e o bloco INTEIRO era descartado:
		// os ultimos ate 4096/recordSize registros sumiam em silencio
		// (com "errors: 1" facil de ignorar). Corrigido aqui.
		if lim := int64(e.recordCount)*int64(e.recordSize) - run.vcn*e.clusterSize; lim < runBytes {
			runBytes = lim
		}
		if runBytes <= 0 {
			continue
		}
		var done int64
		emitted := 0
		for done < runBytes {
			nb := int64(len(buf))
			if nb > runBytes-done {
				nb = runBytes - done
			}
			// alinha a leitura a múltiplos de recordSize (raw I/O alinhado)
			nb -= nb % int64(e.recordSize)
			if nb <= 0 {
				break
			}
			off := run.lcn*e.clusterSize + done
			if err := e.readFn(off, buf[:nb]); err != nil {
				e.recErr(fmt.Sprintf("ler MFT @%d: %v", off, err))
				done += nb
				continue
			}
			chunk := buf[:nb]
			if len(pending) > 0 {
				chunk = append(pending, chunk...)
			}
			full := len(chunk) / e.recordSize
			for i := 0; i < full; i++ {
				idx := recStart + emitted + i
				if idx >= e.recordCount {
					break
				}
				if idx >= startIdx {
					e.parseRecord(idx, chunk[i*e.recordSize:(i+1)*e.recordSize])
					parsed++
				}
			}
			tail := chunk[full*e.recordSize:]
			pending = append(pending[:0], tail...)
			emitted += full
			done += nb
			if time.Since(lastTick) > 3*time.Second {
				lastTick = time.Now()
				fmt.Fprintf(os.Stderr, "\r  MFT: %s registros   ", fmtInt(int64(parsed)))
			}
		}
	}
	return parsed
}

// extendRuns - acrescenta extents de continuação conhecidos (contExt) à
// runlist, em ordem de VCN, enquanto fecharem o buraco no fim. Retorna a
// nova runlist e quantos extents foram adicionados.
func (e *mftEngine) extendRuns(runs []mftRun, dataSize int64) ([]mftRun, int) {
	need := dataSize / e.clusterSize
	covered := runsContiguousVCN(runs)
	if covered >= need {
		return runs, 0
	}
	keys := make([]int64, 0, len(e.contExt))
	for k := range e.contExt {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := append([]mftRun(nil), runs...)
	added := 0
	for _, k := range keys {
		if k < covered {
			continue
		}
		if k > covered {
			break
		}
		ext := e.contExt[k]
		var c int64
		for _, r := range ext {
			c += r.clusters
		}
		if c == 0 {
			continue
		}
		out = append(out, ext...)
		covered += c
		added++
		if covered >= need {
			break
		}
	}
	return out, added
}

// nameUtf8 - decodifica o nome do registro (UTF-16LE armazenado cru).
func (e *mftEngine) nameUtf8(i int) string {
	off := int(e.nameOff[i])
	l := int(e.nameLenB[i])
	if l == 0 || off+l > len(e.nameBuf) {
		return ""
	}
	return utf16Bytes(e.nameBuf[off : off+l])
}

// pathOf - caminho completo de um registro (sobe até a raiz).
func (e *mftEngine) pathOf(i int) string {
	if i == rootRecIdx {
		return e.rootName
	}
	var parts []string
	guard := 0
	for cur := i; cur != rootRecIdx; {
		if cur <= 0 || cur >= e.recordCount || guard > 128 {
			break
		}
		parts = append(parts, e.nameUtf8(cur))
		cur = int(e.parentU32[cur])
		guard++
	}
	base := strings.TrimSuffix(e.rootName, `\`)
	var sb strings.Builder
	sb.WriteString(base)
	for j := len(parts) - 1; j >= 0; j-- {
		sb.WriteString(`\`)
		sb.WriteString(parts[j])
	}
	return sb.String()
}

// addExtKey - registra (extensão, tamanho) a partir do nome em UTF-16LE cru,
// SEM converter para string (o mapa usa chave de tamanho fixo).
func (e *mftEngine) addExtKey(nameB []byte, size int64) {
	k := extKeyOfUTF16(nameB)
	s := e.extMap[k]
	if s == nil {
		s = &extStat{}
		e.extMap[k] = s
	}
	s.Count++
	s.Bytes += size
}

type idxTop struct {
	k     int
	sizes []int64
	items []int32
}

func newIdxTop(k int, sizes []int64) *idxTop {
	return &idxTop{k: k, sizes: sizes, items: make([]int32, 0, k+1)}
}

func (t *idxTop) offer(i int32) {
	if t.k <= 0 {
		return
	}
	if len(t.items) < t.k {
		t.items = append(t.items, i)
		j := len(t.items) - 1
		for j > 0 {
			par := (j - 1) / 2
			if t.sizes[t.items[par]] <= t.sizes[t.items[j]] {
				break
			}
			t.items[par], t.items[j] = t.items[j], t.items[par]
			j = par
		}
		return
	}
	if t.sizes[i] <= t.sizes[t.items[0]] {
		return
	}
	t.items[0] = i
	j := 0
	ln := len(t.items)
	for {
		l, r := 2*j+1, 2*j+2
		m := j
		if l < ln && t.sizes[t.items[l]] < t.sizes[t.items[m]] {
			m = l
		}
		if r < ln && t.sizes[t.items[r]] < t.sizes[t.items[m]] {
			m = r
		}
		if m == j {
			break
		}
		t.items[m], t.items[j] = t.items[j], t.items[m]
		j = m
	}
}

func (t *idxTop) sortedDesc() []int32 {
	out := make([]int32, len(t.items))
	copy(out, t.items)
	sort.Slice(out, func(a, b int) bool { return t.sizes[out[a]] > t.sizes[out[b]] })
	return out
}

func (e *mftEngine) fillReport(r *Report) {
	r.Engine = "mft"
	r.Root = e.rootName
	r.Started = e.started.Format(time.RFC3339)
	r.Duration = time.Since(e.started).Seconds()

	r.Files = int64(e.filesAgg[rootRecIdx])
	r.Dirs = int64(e.subdirsAgg[rootRecIdx]) + 1
	r.Reparse = e.reparse
	r.Errors = e.errN
	r.Bytes = e.sizeL[rootRecIdx]
	r.Alloc = e.sizeA[rootRecIdx]
	r.Deleted = e.deleted
	r.Unnamed = e.unnamed
	r.Orphans = e.orphans
	r.ExtRecs = e.extRecs
	r.ErrList = e.errList
	r.MftSegs = e.mftSegs
	r.MftRuns = e.mftRuns
	r.MftDataBytes = e.mftDataRaw
	r.MftRunBytes = e.mftRunBytes
	r.RecsRead = e.recsRead
	r.RecsTotal = e.recordCount

	// top dirs / top files / extensões
	dirTop := newIdxTop(e.topN, e.sizeL)
	fileTop := newIdxTop(e.topN, e.sizeL)
	for i := 1; i < e.recordCount; i++ {
		f := e.flags[i]
		if f&recNamed == 0 || i == rootRecIdx {
			continue
		}
		if f&recDir != 0 {
			dirTop.offer(int32(i))
		} else {
			fileTop.offer(int32(i))
			e.addExtKey(e.nameBuf[e.nameOff[i]:e.nameOff[i]+uint32(e.nameLenB[i])], e.sizeL[i])
		}
	}
	for _, i := range dirTop.sortedDesc() {
		p := e.pathOf(int(i))
		r.TopDirs = append(r.TopDirs, DirInfo{
			Size:    e.sizeL[i],
			Alloc:   e.sizeA[i],
			Files:   int64(e.filesAgg[i]),
			Subdirs: int64(e.subdirsAgg[i]),
			Path:    p,
			Depth:   uint16(strings.Count(p, `\`)),
		})
	}
	for _, i := range fileTop.sortedDesc() {
		r.TopFiles = append(r.TopFiles, NameSize{Size: e.sizeL[i], Path: e.pathOf(int(i))})
	}
	r.Exts = extTopKey(e.extMap, 50)
}

// dirPaths - caminho completo de TODOS os diretórios em UMA passada.
//
// Antes: e.pathOf(i) por diretório, reconstruindo a cadeia de pais a cada vez
// (O(profundidade) + alocações) -> 405 k dirs x ~8 níveis em C:.
// Agora: DFS sobre o índice de filhos já construído em build(), com caminho
// incremental: O(n) e UMA string por diretório.
func (e *mftEngine) dirPaths() []string {
	paths := make([]string, e.recordCount)
	paths[rootRecIdx] = e.rootName
	stack := make([]int32, 0, 1024)
	stack = append(stack, rootRecIdx)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		pn := paths[n]
		for c := e.childStart[n]; c < e.childStart[n+1]; c++ {
			ch := e.childList[c]
			if e.flags[ch]&recDir == 0 {
				continue
			}
			paths[ch] = joinPath(pn, e.nameUtf8(int(ch)))
			stack = append(stack, ch)
		}
	}
	return paths
}

// writeDirsCSV - exporta todos os diretórios (ordenado por tamanho).
func (e *mftEngine) writeDirsCSV(path string) error {
	fw, err := createCSV(path)
	if err != nil {
		return err
	}
	defer fw.Close()

	paths := e.dirPaths()

	rows := make([]int32, 0, 1024)
	for i := 1; i < e.recordCount; i++ {
		if e.flags[i]&(recNamed|recDir) == (recNamed | recDir) {
			rows = append(rows, int32(i))
		}
	}
	sort.Slice(rows, func(a, b int) bool { return e.sizeL[rows[a]] > e.sizeL[rows[b]] })

	row := make([]string, 5)
	for _, i := range rows {
		row[0] = paths[i]
		row[1] = strconv.FormatInt(e.sizeL[i], 10)
		row[2] = strconv.FormatInt(e.sizeA[i], 10)
		row[3] = strconv.FormatInt(int64(e.filesAgg[i]), 10)
		row[4] = strconv.FormatInt(int64(e.subdirsAgg[i]), 10)
		if err := fw.Write(row); err != nil {
			return err
		}
	}
	return nil
}
