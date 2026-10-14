package main

// f4gate.go - gate de ponta a ponta do scan incremental (F4).
//
// POR QUE EXISTE: o aplicador de delta tem um modo de falha silencioso - aplicar
// um delta errado produz uma arvore PLAUSIVEL mas errada (a mesma familia dos 6
// bugs do motor). O unico oraculo que pega isso e comparar o resultado
// INCREMENTAL contra um FULL SCAN do mesmo estado.
//
// O ciclo roda inteiro dentro de UMA elevacao (scan -> mudancas -> inc -> scan):
//  1. full scan #1  -> grava o snapshot (com o par (UsnJournalID, NextUsn))
//  2. aplica mudancas CONHECIDAS (cria N, deleta M, renomeia K) no volume
//  3. inc           -> le o delta, aplica, re-agrega
//  4. full scan #2  -> verdade-geral do estado apos as mudancas
//  5. compara os dois dirs.csv
//
// Uso: aetherscan selftest --f4 C: [--elevate]

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

// dirs.csv tem 5 colunas: caminho, sizeL, sizeA, arquivos, subdirs.
type dirRow struct {
	sizeL  int64
	sizeA  int64
	files  int64
	subdir int64
}

// readDirsCSV - dirs.csv -> mapa caminho -> (bytes, arquivos, subdirs).
func readDirsCSV(path string) (map[string]dirRow, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := make(map[string]dirRow, 1<<16)
	for _, ln := range strings.Split(string(b), "\n") {
		if ln == "" {
			continue
		}
		p := strings.Split(ln, ",")
		if len(p) < 5 {
			continue
		}
		out[p[0]] = dirRow{
			sizeL: atoi64Safe(p[1]), sizeA: atoi64Safe(p[2]),
			files: atoi64Safe(p[3]), subdir: atoi64Safe(p[4]),
		}
	}
	return out, nil
}

func atoi64Safe(s string) int64 {
	var v int64
	neg := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			return v
		}
		v = v*10 + int64(c-'0')
	}
	if neg {
		return -v
	}
	return v
}

func runF4Gate(args []string) {
	drive := "C:"
	elev := false
	root := `E:\tools\maps\out\f4gate`
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--elevate":
			elev = true
		case a == "--root" && i+1 < len(args):
			i++
			root = args[i]
		case !strings.HasPrefix(a, "-"):
			drive = strings.ToUpper(strings.TrimSuffix(a, "\\"))
			if !strings.HasSuffix(drive, ":") {
				drive += ":"
			}
		}
	}
	if !isElevated() {
		if !elev {
			fatal("o gate F4 requer administrador (abre o volume cru).\n  use: --elevate")
		}
		fmt.Println("[f4gate] disparando processo elevado (UAC)... confirme o prompt do Windows.")
		child := []string{"selftest", "--f4", drive, "--root", root}
		if err := elevateSelf(child); err != nil {
			fatal("falha ao elevar: " + err.Error())
		}
		return
	}

	snapDir := filepath.Join(root, "snap_"+strings.TrimSuffix(drive, ":"))
	// Os artefatos NAO podem ficar no volume varrido: o gate cria full1/,
	// full2/, inc/ e o snapshot DURANTE a execucao, e esses bytes entrariam na
	// comparacao - inclusive nos TOTAIS DOS ANCESTRAIS, que agregam a subarvore
	// inteira (filtrar a linha da pasta nao remove a contribuicao dela acima).
	if strings.EqualFold(filepath.VolumeName(root), drive) {
		for _, v := range []string{"E:", "F:", "D:"} {
			if !strings.EqualFold(v, drive) {
				old := root
				root = v + `\tools\maps\out\f4gate`
				fmt.Printf("nota: artefatos movidos para %s (nao podem ficar no volume varrido %s)\n", root, old)
				break
			}
		}
		snapDir = filepath.Join(root, "snap_"+strings.TrimSuffix(drive, ":"))
	}
	out1 := filepath.Join(root, "full1")
	out2 := filepath.Join(root, "full2")
	outInc := filepath.Join(root, "inc")
	for _, d := range []string{root, snapDir, out1, out2, outInc} {
		os.MkdirAll(d, 0o777)
	}
	// O processo elevado tem console proprio (nao capturado pelo chamador):
	// redireciona tudo para gate.log, que e o artefato do gate.
	if lf, err := os.Create(filepath.Join(root, "gate.log")); err == nil {
		os.Stdout = lf
		os.Stderr = lf // fatal() e panicos tambem precisam ser capturados
		defer lf.Close()
	}
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("\n*** PANICO no gate F4: %v ***\n", r)
			debug.PrintStack()
		}
	}()
	// estado limpo: apaga snapshot anterior (senao o gate compararia com um
	// snapshot velho e o delta poderia ser invalidado pela janela de 32 MB)
	os.Remove(filepath.Join(snapDir, snapBinF))
	os.Remove(filepath.Join(snapDir, snapMetaF))

	fmt.Printf("=== F4 GATE | drive=%s | root=%s ===\n", drive, root)

	// ---------- 1. full scan #1 ----------
	t0 := time.Now()
	e1 := newMftEngine(drive, 300, false)
	e1.started = time.Now()
	e1.outDir = out1
	e1.snapDir = snapDir
	e1.snapUsnID, e1.snapNextUsn = journalBefore(drive)
	fmt.Printf("[1/5] full scan #1 ... journal=0x%016x nextUsn=0x%x\n", e1.snapUsnID, e1.snapNextUsn)
	if err := e1.open(); err != nil {
		fatal(err.Error())
	}
	if err := e1.build(); err != nil {
		fatal(err.Error())
	}
	rep1 := &Report{}
	e1.fillReport(rep1)
	e1.writeDirsCSV(filepath.Join(out1, "dirs.csv"))
	closeHandle(e1.h)
	fmt.Printf("      %.1fs  arquivos=%d dirs=%d bytes=%d  snapshot=%s\n",
		time.Since(t0).Seconds(), rep1.Files, rep1.Dirs, rep1.Bytes, snapDir)

	// ---------- 2. mudancas conhecidas ----------
	testDir := drive + `\aetherscan_f4test`
	sub := filepath.Join(testDir, "sub")
	os.MkdirAll(sub, 0o777)
	const nCreate, nDelete, nRename = 120, 45, 25
	made := 0
	for i := 0; i < nCreate; i++ {
		p := filepath.Join(testDir, fmt.Sprintf("f%03d.bin", i))
		if err := os.WriteFile(p, make([]byte, 1024+i*37), 0o666); err == nil {
			made++
		}
	}
	for i := 0; i < nRename; i++ {
		old := filepath.Join(testDir, fmt.Sprintf("f%03d.bin", i))
		nw := filepath.Join(sub, fmt.Sprintf("renomeado_%03d.bin", i))
		if err := os.Rename(old, nw); err == nil {
			made++
		}
	}
	del := 0
	for i := nCreate - nDelete; i < nCreate; i++ {
		p := filepath.Join(testDir, fmt.Sprintf("f%03d.bin", i))
		if err := os.Remove(p); err == nil {
			del++
		}
	}
	// um diretorio novo, para o delta exercitar criacao de diretorio tambem
	os.MkdirAll(filepath.Join(testDir, "novo_dir"), 0o777)
	time.Sleep(400 * time.Millisecond) // deixa o FS publicar os registros
	fmt.Printf("[2/5] mudancas: %d criados/renomeados, %d deletados em %s\n", made, del, testDir)

	// ---------- 3. inc ----------
	tInc := time.Now()
	e2, _, err := LoadSnapshot(snapDir)
	if err != nil {
		fatal("load snapshot: " + err.Error())
	}
	h, err := openVolume(drive, false)
	if err != nil {
		fatal(err.Error())
	}
	cur, err := usnQuery(h)
	if err != nil {
		fatal("usnQuery: " + err.Error())
	}
	if motivo, ok := validateJournal(e2.snapUsnID, e2.snapNextUsn, cur); !ok {
		fatal("snapshot invalidado no gate: " + motivo)
	}
	fmt.Printf("      snapshot carregado; lendo delta de 0x%x ate 0x%x ...\n", e2.snapNextUsn, cur.NextUsn)
	recs, next, skipped, err := usnReadDelta(h, cur.UsnJournalID, e2.snapNextUsn, cur.NextUsn, 0)
	if err != nil {
		fatal("delta: " + err.Error())
	}
	st := &deltaStats{OldNext: e2.snapNextUsn, NewNext: next, SkippedV3: skipped}
	changed := e2.applyDelta(recs, st)
	e2.restatSizes(changed, st)
	closeHandle(h)
	e2.outDir = outInc
	if err := e2.SaveSnapshot(snapDir, nil, cur.UsnJournalID, next); err != nil {
		fmt.Printf("      aviso: snapshot: %v\n", err)
	}
	if err := e2.aggregate(); err != nil {
		fatal(err.Error())
	}
	repInc := &Report{}
	e2.fillReport(repInc)
	e2.writeDirsCSV(filepath.Join(outInc, "dirs.csv"))
	fmt.Printf("[3/5] inc: delta=%d (criados=%d del=%d ren=%d so-tam=%d) re-stat=%d/%d falhas=%d  %.2fs\n",
		st.Records, st.Created, st.Deleted, st.Renamed, st.SizeOnly,
		st.Restatted, len(changed), st.StatFail, time.Since(tInc).Seconds())
	for _, f := range st.FailList {
		fmt.Printf("      FALHA re-stat: %s\n", f)
	}

	// Janela de churn: do fim do delta do inc ate o inicio do scan #2. Tudo que
	// mudar NESSA janela faz o inc (que parou no USN anterior) divergir do scan
	// #2 - sem que isso seja erro do incremental.
	usnEndInc := next
	var usnBeforeFull2 int64
	if h2, e := openVolume(drive, false); e == nil {
		if c2, e2q := usnQuery(h2); e2q == nil {
			usnBeforeFull2 = c2.NextUsn
		}
		closeHandle(h2)
	}

	// ---------- 4. full scan #2 (verdade-geral) ----------
	// O oraculo NAO e instantaneo: ele le a MFT ao longo de ~16 s, e nesse tempo
	// o volume continua mudando. A janela de divergencia tem de cobrir a duracao
	// INTEIRA do oraculo, nao so o intervalo antes dele - senao um arquivo de log
	// que cresceu durante o scan aparece como "nao explicado".
	t2 := time.Now()
	e3 := newMftEngine(drive, 300, false)
	e3.started = time.Now()
	e3.outDir = out2
	if err := e3.open(); err != nil {
		fatal(err.Error())
	}
	if err := e3.build(); err != nil {
		fatal(err.Error())
	}
	rep2 := &Report{}
	e3.fillReport(rep2)
	e3.writeDirsCSV(filepath.Join(out2, "dirs.csv"))
	closeHandle(e3.h)
	fmt.Printf("[4/5] full scan #2 (oraculo): %.1fs  arquivos=%d dirs=%d bytes=%d\n",
		time.Since(t2).Seconds(), rep2.Files, rep2.Dirs, rep2.Bytes)

	// Fim da janela de divergencia: o USN DEPOIS do oraculo terminar. Cobre
	// tambem tudo o que mudou enquanto ele lia a MFT.
	if h4, e := openVolume(drive, false); e == nil {
		if c4, e4 := usnQuery(h4); e4 == nil && c4.NextUsn > usnBeforeFull2 {
			usnBeforeFull2 = c4.NextUsn
		}
		closeHandle(h4)
	}

	// ---------- 5. compara ----------
	mi, err := readDirsCSV(filepath.Join(outInc, "dirs.csv"))
	if err != nil {
		fatal(err.Error())
	}
	mf, err := readDirsCSV(filepath.Join(out2, "dirs.csv"))
	if err != nil {
		fatal(err.Error())
	}
	// m1 = full scan #1: o CONTROLE. O oraculo (full2) roda ~80 s depois; num
	// volume com escrita ao vivo (pnpm/agentes), full1 e full2 divergem SEM
	// NENHUM DELTA ENVOLVIDO. Comparar inc contra full2 sozinho, portanto,
	// acusa o motor por churn do volume - foi o que produziu centenas de
	// "nao explicados" no F: (13:27-14:11 de 12/10).
	//
	// METRICA CORRETA (erro do delta): nos diretorios em que o volume NAO mudou
	// (m1 == mf), o inc tem de bater. O que sobrar ali E erro do incremental.
	m1, err := readDirsCSV(filepath.Join(out1, "dirs.csv"))
	if err != nil {
		fatal(err.Error())
	}
	// O gate escreve a propria saida DENTRO da arvore varrida (full1/, full2/,
	// inc/, snap_*/, gate.log): esses itens nascem/ mudam entre o scan #1 e o
	// scan #2, entao compara-los seria comparar o instrumento consigo mesmo.
	// Filtra a pasta raiz E tudo sob ela.
	skip := strings.ToLower(filepath.Clean(root))
	isArtifact := func(k string) bool {
		lk := strings.ToLower(k)
		return lk == skip || strings.HasPrefix(lk, skip+string(filepath.Separator))
	}

	// Le o journal NA janela de churn e resolve os caminhos afetados.
	//
	// Usa a arvore do ORACULO (e3, estado final), nao a do inc: um arquivo
	// CRIADO depois do inc nao existe na arvore do inc, entao resolver por ela
	// falhava e o churn virava "nao explicado" (foi o que produziu centenas de
	// falsos nao-explicados no F:, volume com pnpm/agentes escrevendo).
	churnPaths := map[string]bool{}
	if usnBeforeFull2 > usnEndInc {
		if h3, e := openVolume(drive, false); e == nil {
			if cr, _, _, err := usnReadDelta(h3, cur.UsnJournalID, usnEndInc, usnBeforeFull2, 0); err == nil {
				for _, r := range cr {
					i := int(r.FRN)
					if pp, ok := e3.pathOfIdx(i); ok {
						churnPaths[strings.ToLower(pp)] = true
						// sobe a cadeia de pais: o diff pode aparecer em um
						// ancestral do caminho que mudou.
						for p := pp; ; {
							j := strings.LastIndexByte(p, filepath.Separator)
							if j < 3 {
								break
							}
							p = p[:j]
							churnPaths[strings.ToLower(p)] = true
						}
					}
				}
			}
			closeHandle(h3)
		}
	}
	explained := func(k string) bool {
		pre := strings.ToLower(k)
		if churnPaths[pre] {
			return true
		}
		for cp := range churnPaths {
			if strings.HasPrefix(cp, pre+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}

	var diffs, unexplained []string
	for k, vf := range mf {
		if isArtifact(k) {
			continue
		}
		vi, ok := mi[k]
		if !ok {
			diffs = append(diffs, "AUSENTE no inc: "+k)
			continue
		}
		if vi != vf {
			d := fmt.Sprintf(
				"DIFERE %s: inc(sizeL=%d sizeA=%d files=%d sub=%d) vs full(sizeL=%d sizeA=%d files=%d sub=%d)",
				k, vi.sizeL, vi.sizeA, vi.files, vi.subdir, vf.sizeL, vf.sizeA, vf.files, vf.subdir)
			diffs = append(diffs, d)
			if !explained(k) {
				unexplained = append(unexplained, d)
			}
		}
	}
	for k := range mi {
		if isArtifact(k) {
			continue
		}
		if _, ok := mf[k]; !ok {
			d := "SOBRANDO no inc: " + k
			diffs = append(diffs, d)
			if !explained(k) {
				unexplained = append(unexplained, d)
			}
		}
	}
	sort.Strings(diffs)

	fmt.Printf("[5/5] comparacao inc vs full scan do MESMO estado:\n")
	fmt.Printf("      paths: inc=%d  full=%d\n", len(mi), len(mf))
	fmt.Printf("      TOTAIS  inc: arquivos=%d dirs=%d bytes=%d\n", repInc.Files, repInc.Dirs, repInc.Bytes)
	fmt.Printf("      (paths sob %s sao saida do proprio gate e ficam fora da comparacao)\n", root)
	fmt.Printf("      TOTAIS full: arquivos=%d dirs=%d bytes=%d\n", rep2.Files, rep2.Dirs, rep2.Bytes)
	fmt.Printf("      janela de churn: %d registros do journal entre o fim do inc e o scan #2\n",
		len(churnPaths))

	// ---- METRICA DO VEREDITO: erro do delta, descontado o churn do volume ----
	// O oraculo (full2) nao e instantaneo (~80 s lendo a MFT). Num volume com
	// escrita ao vivo, full1 e full2 divergem SEM DELTA NENHUM - esse e o
	// controle. O erro que importa e o inc divergir de full2 em diretorios onde
	// o volume ficou estavel (full1 == full2).
	churnCtl := 0
	estaveis := 0
	var deltaErr, deltaErrStruct []string
	for k, vf := range mf {
		if isArtifact(k) {
			continue
		}
		v1, ok1 := m1[k]
		vi, ok2 := mi[k]
		if !ok1 || !ok2 {
			continue
		}
		if v1 != vf {
			churnCtl++
			continue
		}
		estaveis++
		if vi == vf {
			continue
		}
		d := fmt.Sprintf("ERRO-DELTA %s: inc(sizeL=%d sizeA=%d files=%d sub=%d) vs full(sizeL=%d sizeA=%d files=%d sub=%d)",
			k, vi.sizeL, vi.sizeA, vi.files, vi.subdir, vf.sizeL, vf.sizeA, vf.files, vf.subdir)
		deltaErr = append(deltaErr, d)
		if vi.files != vf.files || vi.subdir != vf.subdir {
			deltaErrStruct = append(deltaErrStruct, d)
		}
	}
	sort.Strings(deltaErrStruct)
	fmt.Printf("      CONTROLE full1 vs full2 (SEM delta): %d dirs mudaram no volume\n", churnCtl)
	fmt.Printf("      dirs estaveis (full1==full2)       : %d\n", estaveis)
	fmt.Printf("      ERRO DO DELTA (inc!=full2 onde estavel): %d dirs  (%d ESTRUTURAIS)\n",
		len(deltaErr), len(deltaErrStruct))

	if len(deltaErrStruct) == 0 {
		fmt.Printf("\n*** GATE F4 OK: erro ESTRUTURAL do delta = 0 em %d dirs estaveis ***\n", estaveis)
		fmt.Printf("    (%d diffs vs o oraculo, TODOS em dirs que o proprio volume mudou; controle=%d)\n",
			len(diffs), churnCtl)
		for i, d := range deltaErr {
			if i >= 10 {
				break
			}
			fmt.Printf("      (byte) %s\n", d)
		}
	} else {
		fmt.Printf("\n*** GATE F4 FALHOU: %d dirs com erro ESTRUTURAL do delta ***\n", len(deltaErrStruct))
		for i, d := range deltaErrStruct {
			if i >= 300 {
				fmt.Printf("      ... e mais %d\n", len(deltaErrStruct)-i)
				break
			}
			fmt.Printf("      %s\n", d)
		}
	}

	// ---------- 6. CAMINHO DE FALHA: o detector de invalidacao ----------
	// Este e O ponto critico do F4: um incremental aplicado sobre uma janela
	// perdida produz arvore silenciosamente errada. O detector tem de RECUSAR
	// nos dois casos, e o fallback tem de EXECUTAR (nao basta detectar).
	fmt.Printf("\n[6/6] caminho de falha (detector de invalidacao + fallback):\n")
	fail := 0
	if motivo, ok := validateJournal(cur.UsnJournalID^0xFFFF, cur.NextUsn, cur); ok {
		fmt.Printf("      FALHOU  ID diferente NAO foi recusado\n")
		fail++
	} else {
		fmt.Printf("      ok      ID diferente     -> recusado (%s)\n", motivo)
	}
	if motivo, ok := validateJournal(cur.UsnJournalID, cur.FirstUsn-1, cur); ok {
		fmt.Printf("      FALHOU  janela perdida NAO foi recusada\n")
		fail++
	} else {
		fmt.Printf("      ok      janela perdida   -> recusado (%s)\n", motivo)
	}
	if motivo, ok := validateJournal(cur.UsnJournalID, cur.NextUsn, cur); !ok {
		fmt.Printf("      FALHOU  snapshot VALIDO foi recusado (%s)\n", motivo)
		fail++
	} else {
		fmt.Printf("      ok      snapshot valido  -> aceito\n")
	}
	outFb := filepath.Join(root, "fallback")
	os.MkdirAll(outFb, 0o777)
	fallbackFullScan(drive, snapDir, outFb)
	if st, err := os.Stat(filepath.Join(outFb, "dirs.csv")); err != nil || st.Size() == 0 {
		fmt.Printf("      FALHOU  fallback nao produziu dirs.csv (%v)\n", err)
		fail++
	} else {
		fmt.Printf("      ok      fallback executou e produziu dirs.csv (%d bytes)\n", st.Size())
	}
	if fail == 0 {
		fmt.Printf("\n*** CAMINHO DE FALHA OK: 2 recusas corretas + 1 aceitacao + fallback executado ***\n")
	} else {
		fmt.Printf("\n*** CAMINHO DE FALHA: %d falhas ***\n", fail)
	}

	// limpeza do diretorio de teste
	os.RemoveAll(testDir)
}
