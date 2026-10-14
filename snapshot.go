package main

// snapshot.go - persistencia do estado do motor para o scan incremental (F4).
//
// O que e gravado: os arrays indexados por indice de REGISTRO da MFT. E o mesmo
// espaco de indices que o USN Journal usa (FileReferenceNumber & 0xFFFFFFFFFF
// = indice do registro), entao aplicar um delta e escrever direto nos arrays,
// sem tocar na MFT.
//
// Formato: snapshot.json (metadados legiveis + o par (UsnJournalID, NextUsn))
// + snapshot.bin (arrays crus, little-endian, na ordem abaixo).
//
// O par (UsnJournalID, NextUsn) e o que torna o incremental SEGURO: sem ele nao
// ha como saber se o delta ainda e aplicavel (ver usn.go).

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unsafe"
)

const (
	snapMagic   = "AETHSNAP"
	snapVersion = 1
	snapMetaF   = "snapshot.json"
	snapBinF    = "snapshot.bin"
)

// extSnap - uma linha do by_extension. A chave e o texto da extensao em hex
// (bytes UTF-16 do extKey), para reconstruir o extKey sem ambiguidade.
type extSnap struct {
	KeyHex string `json:"key_hex"`
	Count  int64  `json:"count"`
	Bytes  int64  `json:"bytes"`
}

type snapMeta struct {
	Magic        string    `json:"magic"`
	Version      int       `json:"version"`
	Drive        string    `json:"drive"`
	RootName     string    `json:"root_name"`
	RecordCount  int       `json:"record_count"`
	RecordSize   int       `json:"record_size"`
	ClusterSize  int64     `json:"cluster_size"`
	RawMft       bool      `json:"raw_mft"`
	MftStart     int64     `json:"mft_start"`
	UsnJournalID string    `json:"usn_journal_id"`
	NextUsn      string    `json:"next_usn"`
	SavedAt      string    `json:"saved_at"`
	NameBufLen   int       `json:"name_buf_len"`
	BinBytes     int64     `json:"bin_bytes"`
	InUse        int64     `json:"in_use"`
	Unnamed      int64     `json:"unnamed"`
	Reparse      int64     `json:"reparse"`
	Deleted      int64     `json:"deleted"`
	ExtRecs      int64     `json:"ext_recs"`
	Files        int64     `json:"files"`
	Dirs         int64     `json:"dirs"`
	Bytes        int64     `json:"bytes"`
	Alloc        int64     `json:"alloc"`
	ExtMap       []extSnap `json:"ext_map"`
}

// rawView - reinterpreta um slice tipado como bytes (little-endian x86/amd64).
// Evita 3M x 6 chamadas de encoding/binary na gravacao do snapshot.
func rawView[T any](s []T) []byte {
	if len(s) == 0 {
		return nil
	}
	var z T
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), len(s)*int(unsafe.Sizeof(z)))
}

// SaveSnapshot - grava o estado do motor em dir.
func (e *mftEngine) SaveSnapshot(dir string, rep *Report, usnID uint64, nextUsn int64) error {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	meta := snapMeta{
		Magic: snapMagic, Version: snapVersion,
		Drive: e.drive, RootName: e.rootName,
		RecordCount: e.recordCount, RecordSize: e.recordSize,
		ClusterSize: e.clusterSize, RawMft: e.rawMft, MftStart: e.mftStart,
		UsnJournalID: fmt.Sprintf("0x%016x", usnID),
		NextUsn:      fmt.Sprintf("0x%016x", uint64(nextUsn)),
		SavedAt:      time.Now().Format(time.RFC3339),
		NameBufLen:   len(e.nameBuf),
		InUse:        e.inUse, Unnamed: e.unnamed, Reparse: e.reparse,
		Deleted: e.deleted, ExtRecs: e.extRecs,
	}
	if rep != nil {
		meta.Files, meta.Dirs, meta.Bytes, meta.Alloc = rep.Files, rep.Dirs, rep.Bytes, rep.Alloc
	}
	for k, v := range e.extMap {
		meta.ExtMap = append(meta.ExtMap, extSnap{
			KeyHex: hex.EncodeToString(k.b[:k.n]), Count: v.Count, Bytes: v.Bytes,
		})
	}

	f, err := os.Create(filepath.Join(dir, snapBinF))
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 4<<20)
	write := func(b []byte) error {
		if len(b) == 0 {
			return nil
		}
		_, err := w.Write(b)
		return err
	}
	var total int64
	for _, b := range [][]byte{
		rawView(e.parentU32), rawView(e.sizeL), rawView(e.sizeA),
		rawView(e.flags), rawView(e.nameOff), rawView(e.nameLenB),
		rawView(e.nameRank), e.nameBuf,
	} {
		if err := write(b); err != nil {
			f.Close()
			return err
		}
		total += int64(len(b))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	meta.BinBytes = total

	b, _ := json.MarshalIndent(meta, "", " ")
	return os.WriteFile(filepath.Join(dir, snapMetaF), b, 0o644)
}

// LoadSnapshot - carrega o estado do motor de dir (arrays ja alocados).
func LoadSnapshot(dir string) (*mftEngine, *snapMeta, error) {
	raw, err := os.ReadFile(filepath.Join(dir, snapMetaF))
	if err != nil {
		return nil, nil, err
	}
	var meta snapMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, nil, err
	}
	if meta.Magic != snapMagic {
		return nil, nil, fmt.Errorf("snapshot invalido (magic=%q)", meta.Magic)
	}
	if meta.Version != snapVersion {
		return nil, nil, fmt.Errorf("snapshot versao %d, esperada %d", meta.Version, snapVersion)
	}

	n := meta.RecordCount
	e := &mftEngine{
		drive: meta.Drive, rootName: meta.RootName,
		recordCount: n, recordSize: meta.RecordSize, clusterSize: meta.ClusterSize,
		rawMft: meta.RawMft, mftStart: meta.MftStart,
		inUse: meta.InUse, unnamed: meta.Unnamed, reparse: meta.Reparse,
		deleted: meta.Deleted, extRecs: meta.ExtRecs,
		// O par do journal TEM de ser restaurado no motor: e o que a validacao
		// (validateJournal) usa para decidir se o delta ainda e aplicavel. Sem
		// isto o snapshot recem-criado se auto-invalida (id = 0).
		snapUsnID:   parseHexU64(meta.UsnJournalID),
		snapNextUsn: int64(parseHexU64(meta.NextUsn)),
		parentU32:   make([]uint32, n), sizeL: make([]int64, n), sizeA: make([]int64, n),
		flags: make([]uint8, n), nameOff: make([]uint32, n), nameLenB: make([]uint16, n),
		nameRank: make([]uint8, n),
		nameBuf:  make([]byte, meta.NameBufLen),
		extMap:   make(map[extKey]*extStat, 256),
	}

	f, err := os.Open(filepath.Join(dir, snapBinF))
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 4<<20)
	readInto := func(b []byte) error {
		if len(b) == 0 {
			return nil
		}
		_, err := io.ReadFull(r, b)
		return err
	}
	for _, b := range [][]byte{
		rawView(e.parentU32), rawView(e.sizeL), rawView(e.sizeA),
		rawView(e.flags), rawView(e.nameOff), rawView(e.nameLenB),
		rawView(e.nameRank), e.nameBuf,
	} {
		if err := readInto(b); err != nil {
			return nil, nil, fmt.Errorf("ler snapshot.bin: %w", err)
		}
	}
	for _, es := range meta.ExtMap {
		kb, err := hex.DecodeString(es.KeyHex)
		if err != nil || len(kb) > 16 {
			continue
		}
		var k extKey
		copy(k.b[:], kb)
		k.n = uint8(len(kb))
		e.extMap[k] = &extStat{Count: es.Count, Bytes: es.Bytes}
	}
	return e, &meta, nil
}
