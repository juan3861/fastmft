package main

// inc.go - comando `inc`: re-scan INCREMENTAL via USN Journal (F4).
//
// Fluxo:
//   1. carrega o snapshot (arrays indexados por registro + o par (UsnJournalID, NextUsn))
//   2. consulta o journal ATUAL e VALIDA o snapshot contra ele
//        - invalido -> cai para full scan (o fallback NAO e opcional: um
//          incremental sobre uma janela perdida produz arvore errada em silencio)
//   3. le o delta desde NextUsn e aplica nos arrays
//   4. re-consulta o tamanho dos arquivos alterados (o USN nao carrega tamanho)
//   5. re-agrega a arvore, emite o relatorio e regrava o snapshot
//
// Uso:
//   aetherscan inc C: --snap <dir> [--out <dir>] [--elevate]

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func runInc(args []string) {
	drive := ""
	snap := ""
	out := ""
	elev := false
	i := 0
	for i < len(args) {
		a := args[i]
		val := func() string {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		switch {
		case a == "--snap" || a == "--snapshot":
			snap = val()
			i += 2
		case strings.HasPrefix(a, "--snap="):
			snap = strings.TrimPrefix(a, "--snap=")
			i++
		case a == "--out":
			out = val()
			i += 2
		case strings.HasPrefix(a, "--out="):
			out = strings.TrimPrefix(a, "--out=")
			i++
		case a == "--elevate":
			elev = true
			i++
		case a == "--child-inc":
			i++ // marcador do relancamento elevado; ignorado
		case !strings.HasPrefix(a, "-"):
			drive = a
			i++
		default:
			fatal("flag desconhecida: " + a)
		}
	}
	if drive == "" || snap == "" {
		fatal("uso: aetherscan inc <drive> --snap <pasta-snapshot> [--out <pasta>] [--elevate]")
	}
	drive = strings.ToUpper(strings.TrimSuffix(drive, "\\"))
	if !strings.HasSuffix(drive, ":") {
		drive += ":"
	}
	if out == "" {
		out = filepath.Join(`E:\tools\maps\out\scans`, "inc_"+strings.TrimSuffix(drive, ":"))
	}
	os.MkdirAll(out, 0o777)

	if !isElevated() {
		if !elev {
			fatal("o comando inc requer administrador (abre o volume cru).\n  use: --elevate")
		}
		fmt.Println("[inc] disparando processo elevado (UAC)... confirme o prompt do Windows.")
		child := []string{"inc", drive, "--snap", snap, "--out", out, "--child-inc"}
		if err := elevateSelf(child); err != nil {
			fatal("falha ao elevar: " + err.Error())
		}
		return
	}

	t0 := time.Now()

	// ---- 1. snapshot ----
	e, meta, err := LoadSnapshot(snap)
	if err != nil {
		fatal("carregar snapshot: " + err.Error())
	}
	e.started = time.Now()
	e.outDir = out
	savedID := parseHexU64(meta.UsnJournalID)
	savedNext := int64(parseHexU64(meta.NextUsn))
	tLoad := time.Since(t0)
	fmt.Printf("[inc] snapshot: %s  registros=%d  journal=%s  nextUsn=%s  (%.3fs)\n",
		snap, meta.RecordCount, meta.UsnJournalID, meta.NextUsn, tLoad.Seconds())

	// ---- 2. valida contra o journal atual ----
	h, err := openVolume(drive, false)
	if err != nil {
		fatal("abrir volume: " + err.Error())
	}
	defer closeHandle(h)
	cur, err := usnQuery(h)
	if err != nil {
		fmt.Printf("[inc] journal indisponivel (%v) -> full scan\n", err)
		fallbackFullScan(drive, snap, out)
		return
	}
	fmt.Printf("[inc] journal atual: id=0x%016x first=0x%x next=0x%x max=%d MB\n",
		cur.UsnJournalID, cur.FirstUsn, cur.NextUsn, cur.MaximumSize/(1<<20))

	if motivo, ok := validateJournal(savedID, savedNext, cur); !ok {
		fmt.Printf("[inc] SNAPSHOT INVALIDADO: %s\n", motivo)
		fmt.Printf("[inc] -> full scan obrigatorio (um incremental aqui daria arvore errada)\n")
		fallbackFullScan(drive, snap, out)
		return
	}

	// ---- 3. delta ----
	tD := time.Now()
	recs, next, skippedV3, err := usnReadDelta(h, cur.UsnJournalID, savedNext, cur.NextUsn, 0)
	if err != nil {
		fmt.Printf("[inc] erro ao ler delta (%v) -> full scan\n", err)
		fallbackFullScan(drive, snap, out)
		return
	}
	tDelta := time.Since(tD)

	st := &deltaStats{OldNext: savedNext, NewNext: next, SkippedV3: skippedV3}
	tA := time.Now()
	changed := e.applyDelta(recs, st)
	tApply := time.Since(tA)

	// ---- 4. tamanhos (o USN nao os carrega) ----
	tS := time.Now()
	e.restatSizes(changed, st)
	tStat := time.Since(tS)

	// ---- 5. snapshot ANTES de agregar ----
	// aggregate() soma os tamanhos dos filhos DENTRO de sizeL/sizeA: depois dela
	// o estado NAO e mais folha e re-agregar infla. Gravar aqui mantem o
	// snapshot idempotente entre execucoes.
	if err := e.SaveSnapshot(snap, nil, cur.UsnJournalID, next); err != nil {
		fmt.Printf("[inc] aviso: falha ao regravar snapshot: %v\n", err)
	} else {
		fmt.Printf("[inc] snapshot atualizado em %s\n", snap)
	}

	// ---- 6. re-agrega + relatorio ----
	tG := time.Now()
	if err := e.aggregate(); err != nil {
		fatal("agregar: " + err.Error())
	}
	tAgg := time.Since(tG)

	rep := &Report{}
	e.fillReport(rep)

	e.writeDirsCSV(filepath.Join(out, "dirs.csv"))
	writeJSON(filepath.Join(out, "meta.json"), rep)
	txt := renderConsole(rep, out)
	os.WriteFile(filepath.Join(out, "console.log"), []byte(txt), 0o666)

	fmt.Printf("\n[inc] delta: %d registros (%.4fs) | v3+ ignorados=%d\n",
		st.Records, tDelta.Seconds(), st.SkippedV3)
	fmt.Printf("[inc] aplicado: criados=%d deletados=%d renomeados=%d so-tamanho=%d\n",
		st.Created, st.Deleted, st.Renamed, st.SizeOnly)
	fmt.Printf("[inc] re-stat: %d ok, %d falhas (de %d alterados)\n",
		st.Restatted, st.StatFail, len(changed))
	for _, f := range st.FailList {
		fmt.Printf("[inc]   FALHA re-stat: %s\n", f)
	}
	fmt.Printf("[inc] fases: load=%.3fs delta=%.4fs apply=%.4fs stat=%.3fs agg=%.3fs\n",
		tLoad.Seconds(), tDelta.Seconds(), tApply.Seconds(), tStat.Seconds(), tAgg.Seconds())
	fmt.Printf("[inc] TOTAL=%.3fs  (um full scan do C: custa ~17.5s)\n", time.Since(t0).Seconds())
}

func parseHexU64(s string) uint64 {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0
	}
	return v
}

// fallbackFullScan - caminho obrigatorio quando o delta nao e aplicavel.
// Roda o motor normal e regrava o snapshot.
func fallbackFullScan(drive, snap, out string) {
	e := newMftEngine(drive, 300, false)
	e.started = time.Now()
	e.outDir = out
	if err := e.open(); err != nil {
		fatal(err.Error())
	}
	defer closeHandle(e.h)
	usnID, nextUsn := journalBefore(drive)
	if err := e.build(); err != nil {
		fatal(err.Error())
	}
	rep := &Report{}
	e.fillReport(rep)
	e.writeDirsCSV(filepath.Join(out, "dirs.csv"))
	writeJSON(filepath.Join(out, "meta.json"), rep)
	os.WriteFile(filepath.Join(out, "console.log"), []byte(renderConsole(rep, out)), 0o666)
	if err := e.SaveSnapshot(snap, rep, usnID, nextUsn); err != nil {
		fmt.Printf("[inc] aviso: snapshot: %v\n", err)
	}
	fmt.Printf("[inc] full scan concluido em %.1fs; snapshot regravado\n", rep.Duration)
}

// journalBefore - par (UsnJournalID, NextUsn) ANTES da varredura. Registrar o
// USN ANTES garante que o proximo delta cubra tudo o que mudou DURANTE o scan
// (a reaplicacao de um evento ja visto e idempotente).
func journalBefore(drive string) (uint64, int64) {
	h, err := openVolume(drive, false)
	if err != nil {
		return 0, 0
	}
	defer closeHandle(h)
	st, err := usnQuery(h)
	if err != nil {
		return 0, 0
	}
	return st.UsnJournalID, st.NextUsn
}
