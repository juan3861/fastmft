package main

// usn.go - leitura e aplicacao do USN Journal (F4).
//
// Por que isso existe: um full scan relê a MFT inteira (3,1 GB no C: = 17,5 s).
// O journal registra TODA mudança do volume desde um ponto (USN). Se guardarmos
// o par (UsnJournalID, NextUsn) junto com o estado, o re-scan vira:
//   ler o delta (~10^3 registros, sub-milissegundo) + aplicar + re-agregar.
//
// O CAMINHO DE FALHA (e nao e robustez, e CORRECAO): o journal tem uma janela
// finita (MaximumSize = 32 MB nos 4 volumes deste box). Se o snapshot for mais
// velho que a janela, o delta NAO e aplicavel e o incremental produziria uma
// arvore silenciosamente errada. Por isso validateJournal() e obrigatorio:
//   - UsnJournalID mudou  -> o journal foi recriado -> full scan
//   - NextUsn salvo < FirstUsn atual (o journal deu a volta) -> full scan
//
// LIMITE CONHECIDO: USN_RECORD_V2 NAO carrega o tamanho do arquivo. O delta da
// estrutura (nome/pai/existencia) e exato; o TAMANHO so fica exato porque
// re-consultamos (stat) cada arquivo alterado - O(mudancas) chamadas, nao O(n).

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

const (
	fsctlQueryUsnJournal = 0x000900F4
	fsctlReadUsnJournal  = 0x000900BB

	errJournalNotActive = syscall.Errno(1179)
	errJournalDeleted   = syscall.Errno(1178)
	errMoreData         = syscall.Errno(234)

	// reasons (winioctl.h)
	usnReasonDataOverwrite = 0x00000001
	usnReasonDataExtend    = 0x00000002
	usnReasonDataTruncate  = 0x00000004
	usnReasonFileCreate    = 0x00000100
	usnReasonFileDelete    = 0x00000200
	usnReasonRenameOld     = 0x00001000
	usnReasonRenameNew     = 0x00002000
)

type usnJournalState struct {
	UsnJournalID    uint64
	FirstUsn        int64
	NextUsn         int64
	LowestValidUsn  int64
	MaxUsn          int64
	MaximumSize     uint64
	AllocationDelta uint64
}

type readUsnJournalData struct {
	StartUsn          int64
	ReasonMask        uint32
	ReturnOnlyOnClose uint32
	Timeout           uint64
	BytesToWaitFor    uint64
	UsnJournalID      uint64
}

type usnRec struct {
	FRN    uint64
	Parent uint64
	Usn    int64
	Reason uint32
	Attrs  uint32
	Name   string
}

// usnQuery - estado atual do journal.
func usnQuery(h syscall.Handle) (usnJournalState, error) {
	var st usnJournalState
	_, err := deviceIoControl(h, fsctlQueryUsnJournal, nil, 0,
		unsafe.Pointer(&st), uint32(unsafe.Sizeof(st)))
	if err != nil {
		return st, err
	}
	return st, nil
}

// usnReadDelta - le todos os registros de [startUsn, endUsn).
//
// endUsn e OBRIGATORIO: sem ele a leitura PERSEGUE o journal ao vivo e, num
// volume ativo (o C: gera ~10^2 registros/s), pode nunca alcancar o fim - foi
// exatamente o travamento do primeiro gate F4. Limitar ao NextUsn observado na
// consulta torna o delta finito e deterministico.
//
// Retorna os registros, o proximo USN e quantos registros foram ignorados por
// versao != 2 (V3/V4 usam FRN de 128 bits e nao sao aplicaveis aqui).
func usnReadDelta(h syscall.Handle, id uint64, startUsn, endUsn int64, maxRecs int) ([]usnRec, int64, int, error) {
	out := make([]usnRec, 0, 1024)
	skipped := 0
	buf := make([]byte, 4<<20)
	cur := startUsn
	for iter := 0; iter < 1000000; iter++ {
		if endUsn > 0 && cur >= endUsn {
			break
		}
		rd := readUsnJournalData{
			StartUsn:     cur,
			ReasonMask:   0xFFFFFFFF,
			UsnJournalID: id,
		}
		br, err := deviceIoControl(h, fsctlReadUsnJournal, unsafe.Pointer(&rd),
			uint32(unsafe.Sizeof(rd)), unsafe.Pointer(&buf[0]), uint32(len(buf)))
		if err != nil && err != errMoreData {
			return out, cur, skipped, err
		}
		if br < 8 {
			break
		}
		next := int64(binary.LittleEndian.Uint64(buf[:8]))
		off := 8
		for off+60 <= int(br) {
			rl := int(binary.LittleEndian.Uint32(buf[off:]))
			if rl == 0 || off+rl > int(br) {
				break
			}
			major := binary.LittleEndian.Uint16(buf[off+4:])
			if major == 2 {
				r := usnRec{
					FRN:    binary.LittleEndian.Uint64(buf[off+8:]) & 0xFFFFFFFFFFFF,
					Parent: binary.LittleEndian.Uint64(buf[off+16:]) & 0xFFFFFFFFFFFF,
					Usn:    int64(binary.LittleEndian.Uint64(buf[off+24:])),
					Reason: binary.LittleEndian.Uint32(buf[off+40:]),
					Attrs:  binary.LittleEndian.Uint32(buf[off+52:]),
				}
				nl := int(binary.LittleEndian.Uint16(buf[off+56:]))
				no := int(binary.LittleEndian.Uint16(buf[off+58:]))
				if no+nl <= rl && nl%2 == 0 {
					r.Name = utf16Bytes(buf[off+no : off+no+nl])
				}
				out = append(out, r)
				if maxRecs > 0 && len(out) >= maxRecs {
					return out, next, skipped, nil
				}
			} else {
				skipped++
			}
			off += rl
		}
		if next <= cur {
			break
		}
		cur = next
		if endUsn > 0 && cur >= endUsn {
			break
		}
	}
	return out, cur, skipped, nil
}

// validateJournal - o delta ainda e aplicavel a este snapshot?
// Devolve ("", true) quando sim; senao o motivo da invalidacao.
func validateJournal(savedID uint64, savedNext int64, cur usnJournalState) (string, bool) {
	if cur.UsnJournalID != savedID {
		return fmt.Sprintf("UsnJournalID mudou (snapshot 0x%016x, atual 0x%016x)",
			savedID, cur.UsnJournalID), false
	}
	if savedNext > cur.NextUsn {
		return fmt.Sprintf("NextUsn salvo (0x%x) > atual (0x%x)", savedNext, cur.NextUsn), false
	}
	if savedNext < cur.FirstUsn {
		return fmt.Sprintf("journal deu a volta: NextUsn salvo (0x%x) < FirstUsn atual (0x%x) "+
			"-> a janela de 32 MB nao cobre o periodo desde o snapshot", savedNext, cur.FirstUsn), false
	}
	return "", true
}

// deltaStats - o que o delta fez.
type deltaStats struct {
	Records   int
	Created   int
	Deleted   int
	Renamed   int
	SizeOnly  int
	SkippedV3 int
	Restatted int
	StatFail  int
	FailList  []string
	NewRecs   int
	OldNext   int64
	NewNext   int64
}

// growTo - garante arrays com pelo menos n+1 posicoes (novos registros alocados
// apos o snapshot tem indice >= recordCount original).
func (e *mftEngine) growTo(idx int) {
	if idx < len(e.flags) {
		return
	}
	need := idx + 1
	grow := func(n int) int {
		if n < 1024 {
			n = 1024
		}
		return n
	}
	add := grow(need - len(e.flags))
	e.parentU32 = append(e.parentU32, make([]uint32, add)...)
	e.sizeL = append(e.sizeL, make([]int64, add)...)
	e.sizeA = append(e.sizeA, make([]int64, add)...)
	e.flags = append(e.flags, make([]uint8, add)...)
	e.nameOff = append(e.nameOff, make([]uint32, add)...)
	e.nameLenB = append(e.nameLenB, make([]uint16, add)...)
	e.nameRank = append(e.nameRank, make([]uint8, add)...)
	if need > e.recordCount {
		e.recordCount = need
	}
}

// applyDelta - aplica os registros do journal nos arrays.
// Devolve os indices que precisam de re-stat (tamanho nao vem no USN).
func (e *mftEngine) applyDelta(recs []usnRec, st *deltaStats) []int {
	changed := make([]int, 0, len(recs))
	for _, r := range recs {
		st.Records++
		idx := int(r.FRN)
		if idx < 16 && idx != rootRecIdx {
			continue // metadado NTFS
		}
		if r.Reason&usnReasonFileDelete != 0 {
			if idx < len(e.flags) {
				e.flags[idx] &^= recInUse | recNamed | recUnnamed
				changed = append(changed, idx)
				st.Deleted++
			}
			continue
		}
		// criacao / rename / alteracao de dados / fechamento
		e.growTo(idx)
		if idx >= len(e.nameRank) {
			continue
		}
		if r.Name != "" {
			u16 := encodeUTF16(r.Name)
			if idx >= len(e.nameOff) {
				continue
			}
			e.nameOff[idx] = uint32(len(e.nameBuf))
			e.nameLenB[idx] = uint16(len(u16))
			e.nameBuf = append(e.nameBuf, u16...)
			e.nameRank[idx] = 1
		}
		wasNamed := e.flags[idx]&recNamed != 0
		wasDir := e.flags[idx]&recDir != 0
		e.flags[idx] |= recInUse | recNamed
		if r.Attrs&fileAttributeDirectory != 0 {
			e.flags[idx] |= recDir
		} else {
			e.flags[idx] &^= recDir
		}
		if r.Parent != 0 {
			e.parentU32[idx] = uint32(r.Parent)
		}
		changed = append(changed, idx)

		switch {
		case r.Reason&usnReasonRenameNew != 0 || r.Reason&usnReasonRenameOld != 0:
			st.Renamed++
		case !wasNamed || !wasDir && r.Attrs&fileAttributeDirectory != 0:
			st.Created++
		case r.Reason&(usnReasonDataOverwrite|usnReasonDataExtend|usnReasonDataTruncate) != 0:
			st.SizeOnly++
		default:
			st.Created++
		}
	}
	// remove duplicatas preservando a ordem (um arquivo pode ter varios eventos)
	seen := make(map[int]struct{}, len(changed))
	uniq := changed[:0]
	for _, i := range changed {
		if _, ok := seen[i]; ok {
			continue
		}
		seen[i] = struct{}{}
		uniq = append(uniq, i)
	}
	return uniq
}

func encodeUTF16(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		binary.LittleEndian.PutUint16(b[i*2:], v)
	}
	return b
}

// restatSizes - o USN nao carrega tamanho: para cada registro alterado que
// ainda existe, monta o caminho e consulta o tamanho atual. O(mudancas).
func (e *mftEngine) restatSizes(idxs []int, st *deltaStats) {
	var segs [64][]byte
	var buf [4096]byte
	for _, idx := range idxs {
		if e.flags[idx]&(recInUse|recNamed) != (recInUse | recNamed) {
			continue
		}
		p, ok := e.buildPath(idx, segs[:0], buf[:0])
		if !ok {
			st.StatFail++
			if len(st.FailList) < 25 {
				st.FailList = append(st.FailList, fmt.Sprintf("buildPath idx=%d frn-flag=%02x", idx, e.flags[idx]))
			}
			continue
		}
		log, alloc, ok := statPathInfo(p)
		if !ok {
			st.StatFail++
			if len(st.FailList) < 25 {
				st.FailList = append(st.FailList, fmt.Sprintf("stat len=%d %s", len(p), p))
			}
			continue
		}
		if e.flags[idx]&recDir != 0 {
			continue // diretorio: tamanho vem da agregacao dos filhos
		}
		e.sizeL[idx] = log
		e.sizeA[idx] = alloc
		st.Restatted++
	}
}

// pathOfIdx - caminho absoluto do registro idx (wrapper de buildPath).
func (e *mftEngine) pathOfIdx(idx int) (string, bool) {
	var segs [64][]byte
	var buf [4096]byte
	return e.buildPath(idx, segs[:0], buf[:0])
}

// buildPath - caminho absoluto do registro idx, subindo pelos pais.
func (e *mftEngine) buildPath(idx int, segs [][]byte, buf []byte) (string, bool) {
	cur := idx
	for depth := 0; depth < 64; depth++ {
		if cur == rootRecIdx {
			break
		}
		if cur < 0 || cur >= len(e.nameLenB) {
			return "", false
		}
		nb := int(e.nameLenB[cur])
		if nb == 0 || int(e.nameOff[cur])+nb > len(e.nameBuf) {
			return "", false
		}
		off := int(e.nameOff[cur])
		segs = append(segs, e.nameBuf[off:off+nb])
		p := int(e.parentU32[cur])
		if p == cur {
			return "", false
		}
		cur = p
		if cur < 16 && cur != rootRecIdx {
			return "", false
		}
	}
	if len(segs) == 0 {
		return "", false
	}
	buf = append(buf, e.drive...)
	buf = append(buf, '\\')
	for i := len(segs) - 1; i >= 0; i-- {
		if i != len(segs)-1 {
			buf = append(buf, '\\')
		}
		buf = append(buf, utf16Bytes(segs[i])...)
	}
	return string(buf), true
}

// fileStandardInfo - FILE_STANDARD_INFO (winbase.h), 24 bytes com padding.
type fileStandardInfo struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  uint8
	Directory      uint8
	_              [2]byte
}

const fileStandardInfoClass = 1

// statPathInfo - (logico, alocado) do arquivo.
//
// O ALOCADO tem de vir do proprio NTFS, nao de um arredondamento: arquivos
// RESIDENTES (dados dentro do proprio registro da MFT) tem alocado = tamanho
// logico, nao cluster-arredondado. FILE_STANDARD_INFO devolve os dois campos de
// uma vez e com a mesma semantica que o motor le da MFT.
func statPathInfo(path string) (int64, int64, bool) {
	h, err := openFile(path, fileReadAttributes,
		fileShareRead|fileShareWrite|4 /* FILE_SHARE_DELETE */, fileFlagBackupSemantics)
	if err != nil {
		return 0, 0, false
	}
	defer closeHandle(h)
	var fsi fileStandardInfo
	r, _, _ := procGetFileInformationByHandleEx.Call(
		uintptr(h), uintptr(fileStandardInfoClass),
		uintptr(unsafe.Pointer(&fsi)), unsafe.Sizeof(fsi))
	if r == 0 {
		return 0, 0, false
	}
	return fsi.EndOfFile, fsi.AllocationSize, true
}
