package main

// main.go - CLI do AetherScan (motor de mapeamento de discos).
//
//  aetherscan scan <alvo> [flags]
//    --mode auto|walk|mft     motor a usar (default: auto -> mft se elevado, senão walk)
//    --top N                  itens nos rankings (default 300)
//    --out DIR                pasta de saída (default E:\tools\maps\out\scans\...)
//    --files                  exporta a lista completa de arquivos (files.csv)
//    --elevate                (mft) dispara UAC para rodar elevado
//    --idle                   prioridade IDLE (baixa)
//    --workers N              threads do walk (default: nº de CPUs)
//    --child                  (interno) marca processo filho elevado
//
//  aetherscan selftest --fuzz <corpus.bin> [--iters N] [--seed S]
//    (robustez) fuzzing in-process do parser MFT.
//
//  aetherscan version

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// engineVersion - versao do motor. BUMPAR a cada frente fechada: a medicao so
// vale se o binario medido for rastreavel ao fonte (SHA-256 reproduzivel).
const engineVersion = "0.4.1"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "scan":
		runScan(parseScan(args[1:]))
	case "mftfile":
		runMftFile(args[1:])
	case "inc":
		runInc(args[1:])
	case "selftest":
		runSelfTest(args[1:])
	case "version", "--version", "-v":
		fmt.Printf("aetherscan %s (AetherMind) - go%s %s/%s\n", engineVersion, goVer(), runtime.GOOS, runtime.GOARCH)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`AetherScan - motor de mapeamento de discos (AetherMind)

uso:
  aetherscan scan <alvo> [flags]
    <alvo>          C: | C:\ | C:\Usuarios\... (caminho)
    --mode X        auto | walk | mft   (default auto)
    --top N         itens nos rankings (default 300)
    --out DIR       pasta de saída
    --files         exporta files.csv completo (pode ser grande)
    --elevate       (mft) dispara UAC para rodar elevado
    --idle          roda em prioridade IDLE
    --serial        (scan ALL) volta a varrer os volumes UM POR VEZ. O default
                    agora e PARALELO: os 4 volumes sao discos fisicos separados
                    (sem spindle compartilhado) e o paralelo mede 2,4x mais rapido.
    --parallel      (scan ALL) explicito; ja e o default
                    entre discos fisicos diferentes; pede ~1,5-2 GB de RAM)
    --ioread N      MB por bloco de leitura da MFT (default 16; ajustavel)
    --bufmb N       orcamento do pool de buffers por volume, em MB (default 32)
    --nobuf         leitura sem cache (FILE_FLAG_NO_BUFFERING + buffers alinhados)
    --mem-budget N  MB de RAM para volumes em paralelo (default: 60% da livre)
    --workers N     threads do walk (default: nº de CPUs)

  aetherscan version

  aetherscan inc <drive> --snap <pasta-snapshot> [--out DIR] [--elevate]
    (F4) re-scan INCREMENTAL: aplica o delta do USN Journal sobre o snapshot e
    re-agrega. Journal invalidado (ID trocado ou janela perdida) -> cai para full
    scan obrigatorio: um incremental sobre janela perdida daria arvore errada em
    silencio. Exige administrador (abre o volume cru).

  aetherscan selftest --fuzz <corpus.bin> [--iters N] [--seed S]
    (robustez) fuzzing in-process do parser MFT: muta bytes e exige que o motor
    nunca entre em panico nem trave.
  aetherscan selftest --f4 <drive> [--root DIR] [--elevate]
    (F4) gate do incremental: compara inc vs full scan com CONTROLE de churn
    (full1 vs full2) e erro do delta so onde o volume nao mudou.
  aetherscan selftest --iosweep <drive> [--ioreads a,b,c] [--bufmb ...]
    (F1) sweep intercalado A-B-A-B de ioread/bufmb/nobuf, com contadores de I/O.

  aetherscan mftfile <arquivo> [--top N] [--out DIR] [--repeat N]
    (teste offline) le um dump de MFT cru ou imagem de disco NTFS.
    --repeat N repete o build N vezes (iteracoes 2..N quentes no cache) para
    medir a vazao do PARSER sem o ruido de I/O.
    AETHERSCAN_SERIAL=1 forca o caminho serial original (A/B controlado).
`)
}

type scanOpts struct {
	Target   string
	Mode     string
	TopN     int
	Out      string
	Elevate  bool
	Files    bool
	Idle     bool
	Workers  int
	Child    bool
	Find     string
	Parallel bool
	Serial   bool // desliga o paralelismo no alvo ALL (default agora e paralelo)
	IoRead   int  // MB por bloco de leitura (0 = default)
	NoBuf    bool // FILE_FLAG_NO_BUFFERING
	MemMB    int  // orcamento de RAM p/ volumes em paralelo (0 = 60% da livre)
	BufMB    int  // orcamento do pool de buffers por volume em MB (0 = 32)
	Snapshot string // pasta onde gravar o snapshot do estado (F4)
}

// applyIO - aplica os ajustes de I/O que dependem de open() (recordSize).
// NAO mexe em e.nobuf: essa flag decide o CreateFile e por isso e definida
// ANTES do open (e pode ser desligada la, ex.: arquivo nao alinhado ao setor).
func (e *mftEngine) applyIO(o *scanOpts) {
	if o == nil {
		return
	}
	if o.BufMB > 0 {
		e.bufMB = o.BufMB
	}
	if o.Workers > 0 {
		e.workers = o.Workers
	}
	if o.IoRead > 0 && e.recordSize > 0 {
		recs := (o.IoRead << 20) / e.recordSize
		if recs < 16 {
			recs = 16
		}
		if recs > 1<<20 {
			recs = 1 << 20
		}
		e.readRecs = recs
	}
}

func parseScan(args []string) *scanOpts {
	o := &scanOpts{Mode: "auto", TopN: 300}
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
		case a == "--mode":
			o.Mode = val()
			i += 2
		case strings.HasPrefix(a, "--mode="):
			o.Mode = strings.TrimPrefix(a, "--mode=")
			i++
		case a == "--top":
			o.TopN = atoiSafe(val(), 300)
			i += 2
		case strings.HasPrefix(a, "--top="):
			o.TopN = atoiSafe(strings.TrimPrefix(a, "--top="), 300)
			i++
		case a == "--out":
			o.Out = val()
			i += 2
		case strings.HasPrefix(a, "--out="):
			o.Out = strings.TrimPrefix(a, "--out=")
			i++
		case a == "--files":
			o.Files = true
			i++
		case a == "--elevate":
			o.Elevate = true
			i++
		case a == "--idle":
			o.Idle = true
			i++
		case a == "--parallel":
			o.Parallel = true
			i++
		case a == "--serial":
			o.Serial = true
			i++
		case a == "--bufmb":
			o.BufMB = atoiSafe(val(), 0)
			i += 2
		case strings.HasPrefix(a, "--bufmb="):
			o.BufMB = atoiSafe(strings.TrimPrefix(a, "--bufmb="), 0)
			i++
		case a == "--nobuf":
			o.NoBuf = true
			i++
		case a == "--mem-budget":
			o.MemMB = atoiSafe(val(), 0)
			i += 2
		case strings.HasPrefix(a, "--mem-budget="):
			o.MemMB = atoiSafe(strings.TrimPrefix(a, "--mem-budget="), 0)
			i++
		case a == "--ioread":
			o.IoRead = atoiSafe(val(), 0)
			i += 2
		case strings.HasPrefix(a, "--ioread="):
			o.IoRead = atoiSafe(strings.TrimPrefix(a, "--ioread="), 0)
			i++
		case a == "--workers":
			o.Workers = atoiSafe(val(), 0)
			i += 2
		case strings.HasPrefix(a, "--workers="):
			o.Workers = atoiSafe(strings.TrimPrefix(a, "--workers="), 0)
			i++
		case a == "--child":
			o.Child = true
			i++
		case a == "--snapshot":
			o.Snapshot = val()
			i += 2
		case strings.HasPrefix(a, "--snapshot="):
			o.Snapshot = strings.TrimPrefix(a, "--snapshot=")
			i++
		case a == "--find":
			o.Find = val()
			i += 2
		case strings.HasPrefix(a, "--find="):
			o.Find = strings.TrimPrefix(a, "--find=")
			i++
		case !strings.HasPrefix(a, "-"):
			o.Target = a
			i++
		default:
			fatal("flag desconhecida: " + a)
		}
	}
	if o.Target == "" {
		fatal("alvo obrigatorio (ex: aetherscan scan C:)")
	}
	return o
}

func atoiSafe(s string, d int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return d
	}
	return n
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "erro:", msg)
	os.Exit(1)
}

func goVer() string { return strings.TrimPrefix(runtime.Version(), "go") }

func runScan(o *scanOpts) {
	if strings.EqualFold(o.Target, "ALL") {
		runScanAll(o)
		return
	}
	target := normalizeRoot(o.Target)
	drive := driveOf(target)
	if drive == "" {
		fatal("alvo deve ser caminho absoluto (ex: C: ou C:\\pasta)")
	}

	mode := o.Mode
	if mode == "auto" {
		if isDriveRoot(target) && fsNameOf(drive) == "NTFS" {
			mode = "mft"
		} else {
			mode = "walk"
		}
	}

	if mode == "mft" {
		if !isDriveRoot(target) {
			fatal("modo mft exige alvo na raiz do volume (ex: C:)")
		}
		if !isElevated() {
			if o.Elevate {
				out := resolveOut(o, target, mode)
				os.MkdirAll(out, 0o777)
				childArgs := []string{"scan", target, "--mode", "mft", "--top", strconv.Itoa(o.TopN), "--out", out, "--child"}
				if o.Idle {
					childArgs = append(childArgs, "--idle")
				}
				if o.Find != "" {
					childArgs = append(childArgs, "--find", o.Find)
				}
				fmt.Println("[mft] disparando processo elevado (UAC)... confirme o prompt do Windows.")
				if err := elevateSelf(childArgs); err != nil {
					fatal("falha ao elevar: " + err.Error())
				}
				waitChild(out)
				return
			}
			fatal("modo mft requer administrador.\n  opção 1: aetherscan scan " + target + " --mode mft --elevate\n  opção 2: rodar este binário como Administrador")
		}
		runMft(o, target, drive)
		return
	}
	runWalk(o, target)
}

// runScanAll - mapeia TODOS os volumes NTFS numa única elevação (UAC).
func runScanAll(o *scanOpts) {
	if o.Mode != "mft" && o.Mode != "auto" {
		fatal("o alvo ALL só é suportado com --mode mft")
	}
	if !isElevated() {
		if !o.Elevate {
			fatal("o alvo ALL requer administrador.\n  use: aetherscan scan ALL --mode mft --elevate")
		}
		out := resolveOut(o, "ALL", "mft")
		os.MkdirAll(out, 0o777)
		childArgs := []string{"scan", "ALL", "--mode", "mft", "--top", strconv.Itoa(o.TopN), "--out", out, "--child"}
		if o.Idle {
			childArgs = append(childArgs, "--idle")
		}
		if o.Serial {
			childArgs = append(childArgs, "--serial")
		}
		if o.NoBuf {
			childArgs = append(childArgs, "--nobuf")
		}
		if o.IoRead > 0 {
			childArgs = append(childArgs, "--ioread", strconv.Itoa(o.IoRead))
		}
		if o.MemMB > 0 {
			childArgs = append(childArgs, "--mem-budget", strconv.Itoa(o.MemMB))
		}
		fmt.Println("[mft] disparando processo elevado (UAC)... confirme o prompt do Windows.")
		if err := elevateSelf(childArgs); err != nil {
			fatal("falha ao elevar: " + err.Error())
		}
		waitChild(out)
		return
	}
	runMftAll(o)
}

// detectNTFSDrives - lista as letras de volume NTFS acessíveis.
func detectNTFSDrives() []string {
	var out []string
	for c := 'A'; c <= 'Z'; c++ {
		d := string(c) + ":"
		if fsNameOf(d) == "NTFS" {
			out = append(out, d)
		}
	}
	return out
}

// runMftAll - varre cada volume NTFS com o motor MFT, gravando em subpastas.
// Com --parallel os volumes rodam SIMULTANEAMENTE (ver pool.go): o tempo total
// passa de SOMA para ~MAX, sobrepondo o I/O de discos fisicos diferentes.
func runMftAll(o *scanOpts) {
	root := resolveOut(o, "ALL", "mft")
	if err := os.MkdirAll(root, 0o777); err != nil {
		fatal("criar pasta de saída: " + err.Error())
	}
	if o.Idle {
		setPriority(true)
	}
	drives := detectNTFSDrives()
	if len(drives) == 0 {
		fatal("nenhum volume NTFS acessível encontrado")
	}
	fmt.Printf("[mft] volumes NTFS: %v\n", drives)

	t0 := time.Now()
	jobs := make([]volJob, 0, len(drives))
	for _, d := range drives {
		dr := d
		out := filepath.Join(root, strings.TrimSuffix(dr, ":"))
		os.MkdirAll(out, 0o777)
		jobs = append(jobs, volJob{name: dr, fn: func() (volRow, error) {
			e := newMftEngine(dr, o.TopN, false)
			e.started = time.Now()
			e.outDir = out
			e.nobuf = o.NoBuf // antes de open(): a flag vai no CreateFile
			if err := e.open(); err != nil {
				return volRow{}, err
			}
			e.applyIO(o)
			if err := e.build(); err != nil {
				return volRow{}, err
			}
			rep := &Report{}
			e.fillReport(rep)
			txt := renderConsole(rep, out)
			os.WriteFile(filepath.Join(out, "console.log"), []byte(txt), 0o666)
			writeJSON(filepath.Join(out, "meta.json"), rep)
			e.writeDirsCSV(filepath.Join(out, "dirs.csv"))
			writeTopCSV(filepath.Join(out, "top_files.csv"), rep)
			return volRow{
				name: dr, files: rep.Files, dirs: rep.Dirs,
				bytes: rep.Bytes, alloc: rep.Alloc, secs: rep.Duration,
				orphans: rep.Orphans, recsR: int64(rep.RecsRead), recsT: rep.RecsTotal,
				extents: rep.MftSegs, console: txt,
			}, nil
		}})
	}
	// PARALELO POR DEFAULT: os 4 volumes deste box sao DISCOS FISICOS SEPARADOS
	// (C: Kingston SA400, F: MV512GB, E:/D: ST500LM012) -> nao ha spindle
	// compartilhado. Medido: 84,5 s paralelo vs 201,9 s sequencial (2,4x).
	// O orcamento de RAM do pool serializa sozinho quando a memoria aperta.
	rows := runVolJobs(jobs, !o.Serial, o.MemMB)

	var sum strings.Builder
	sum.WriteString("=== AetherScan | MODO ALL (MFT) | resumo ===\n")
	sum.WriteString(fmt.Sprintf("volumes: %d | paralelo: %v | tempo total: %.1fs\n",
		len(drives), !o.Serial, time.Since(t0).Seconds()))
	sum.WriteString(fmt.Sprintf("%-5s %12s %12s %12s %8s %9s\n", "VOL", "ARQUIVOS", "DIRS", "BYTES", "SEG", "ORFAOS"))
	for _, r := range rows {
		if r.name == "" {
			continue
		}
		fmt.Printf("\n[mft] ==== %s ====\n", r.name)
		fmt.Print(r.console)
		sum.WriteString(fmt.Sprintf("%-5s %12s %12s %12s %8.1fs %9d\n",
			r.name, fmtInt(r.files), fmtInt(r.dirs), fmtBytes(r.bytes), r.secs, r.orphans))
	}
	out := sum.String()
	fmt.Print("\n" + out)
	os.WriteFile(filepath.Join(root, "console.log"), []byte(out), 0o666)
	os.WriteFile(filepath.Join(root, "resumo.txt"), []byte(out), 0o666)
	if o.Child {
		os.WriteFile(filepath.Join(root, ".done"), []byte("ok\n"), 0o666)
	}
}

func resolveOut(o *scanOpts, target, mode string) string {	if o.Out != "" {
		abs, err := filepath.Abs(o.Out)
		if err == nil {
			return abs
		}
		return o.Out
	}
	name := strings.ReplaceAll(strings.TrimSuffix(target, `\`), `\`, "_")
	name = strings.ReplaceAll(name, ":", "")
	ts := time.Now().Format("20060102_150405")
	return filepath.Join(`E:\tools\maps\out\scans`, name+"_"+mode+"_"+ts)
}

func runWalk(o *scanOpts, target string) {
	out := resolveOut(o, target, "walk")
	if err := os.MkdirAll(out, 0o777); err != nil {
		fatal("criar pasta de saída: " + err.Error())
	}
	var shardFn func(int) string
	if o.Files {
		shardFn = func(i int) string {
			return filepath.Join(out, fmt.Sprintf("files.part%02d.csv", i))
		}
	}
	w := newWalkers(target, o.Workers, o.TopN, o.Files, shardFn)
	if o.Idle {
		setPriority(true)
	}
	w.run()

	rep := &Report{}
	w.fillReport(rep)

	finish(o, out, rep, func(p string) error { return w.writeDirsCSV(p) })

	if o.Files {
		if err := mergeFileParts(out); err != nil {
			fmt.Fprintln(os.Stderr, "aviso: merge files.csv:", err)
		}
	}
}

func runMft(o *scanOpts, target, drive string) {
	out := resolveOut(o, target, "mft")
	if err := os.MkdirAll(out, 0o777); err != nil {
		fatal("criar pasta de saída: " + err.Error())
	}
	e := newMftEngine(drive, o.TopN, false)
	e.started = time.Now()
	e.outDir = out

	// O motor mft abre o volume cru (\\.\X:) e por isso exige administrador.
	// Com --elevate o proprio binario se relanca elevado (UAC).
	if !isElevated() {
		if o.Elevate {
			child := []string{"scan", drive, "--mode", "mft", "--top", strconv.Itoa(o.TopN), "--out", out, "--child"}
			if o.NoBuf {
				child = append(child, "--nobuf")
			}
			if o.IoRead > 0 {
				child = append(child, "--ioread", strconv.Itoa(o.IoRead))
			}
			if o.Snapshot != "" {
				child = append(child, "--snapshot", o.Snapshot)
			}
			fmt.Println("[mft] disparando processo elevado (UAC)... confirme o prompt do Windows.")
			if err := elevateSelf(child); err != nil {
				fatal("falha ao elevar: " + err.Error())
			}
			return
		}
		fatal("o motor mft requer administrador (abre o volume cru).\n  use: --elevate")
	}
	e.dumpPat = o.Find
	e.nobuf = o.NoBuf // precisa vir ANTES de open(): a flag vai no CreateFile
	if o.Find != "" {
		e.dumpPath = filepath.Join(out, "find.txt")
		e.dumpMax = 200
	}
	if o.Idle {
		setPriority(true)
	}
	if err := e.open(); err != nil {
		fatal(err.Error())
	}
	e.applyIO(o)
	// O par (UsnJournalID, NextUsn) e capturado ANTES da varredura: assim o
	// proximo delta cobre tambem o que mudou DURANTE o scan (reaplicar um evento
	// ja visto e idempotente).
	if o.Snapshot != "" {
		// O par (UsnJournalID, NextUsn) e capturado ANTES da varredura: assim o
		// proximo delta cobre tambem o que mudou DURANTE o scan (reaplicar um
		// evento ja visto e idempotente). A gravacao em si acontece dentro de
		// build(), ANTES da agregacao (o snapshot tem de ser estado folha).
		e.snapDir = o.Snapshot
		e.snapUsnID, e.snapNextUsn = journalBefore(drive)
	}
	if err := e.build(); err != nil {
		fatal(err.Error())
	}
	e.writeDump()

	rep := &Report{}
	e.fillReport(rep)
	if o.Snapshot != "" {
		fmt.Printf("[mft] snapshot gravado em %s (journal=0x%016x nextUsn=0x%x)\n",
			o.Snapshot, e.snapUsnID, e.snapNextUsn)
	}

	finish(o, out, rep, func(p string) error { return e.writeDirsCSV(p) })
}

// runMftFile - testa o motor MFT contra um ou mais arquivos (dump MFT cru ou
// imagem NTFS). Com varios arquivos (ou --parallel) usa o MESMO pool de volumes
// do `scan ALL`, o que permite medir o ganho de sobrepor I/O de discos
// diferentes. --repeat N repete o build N vezes (iteracoes 2..N ficam quentes
// no cache): e assim que se mede a VAZAO DO PARSER sem o ruido de I/O do disco.
func runMftFile(args []string) {
	var paths []string
	var out string
	top := 50
	repeat := 1
	parallel := false
	ioRead := 0
	noBuf := false
	memBudget := 0
	snapOut := ""
	loadSnap := ""
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--top" && i+1 < len(args):
			top = atoiSafe(args[i+1], 50)
			i += 2
		case a == "--out" && i+1 < len(args):
			out = args[i+1]
			i += 2
		case a == "--repeat" && i+1 < len(args):
			repeat = atoiSafe(args[i+1], 1)
			i += 2
		case a == "--parallel":
			parallel = true
			i++
		case a == "--nobuf":
			noBuf = true
			i++
		case a == "--ioread" && i+1 < len(args):
			ioRead = atoiSafe(args[i+1], 0)
			i += 2
		case a == "--mem-budget" && i+1 < len(args):
			memBudget = atoiSafe(args[i+1], 0)
			i += 2
		case a == "--snapshot" && i+1 < len(args):
			snapOut = args[i+1]
			i += 2
		case a == "--load" && i+1 < len(args):
			loadSnap = args[i+1]
			i += 2
		case !strings.HasPrefix(a, "-"):
			paths = append(paths, a)
			i++
		default:
			fatal("flag desconhecida: " + a)
		}
	}
	if loadSnap != "" {
		// Reconstroi o relatorio APENAS a partir do snapshot. Serve de oraculo
		// sem admin: comparar o dirs.csv daqui com o do scan direto prova que a
		// serializacao + a agregacao estao exatas.
		e, _, err := LoadSnapshot(loadSnap)
		if err != nil {
			fatal("carregar snapshot: " + err.Error())
		}
		e.started = time.Now()
		e.recsRead = e.recordCount
		// Grava o snapshot ANTES de agregar: aggregate() muta sizeL/sizeA, entao
		// o estado pos-agregacao nao e folha. Isto torna o ciclo
		// (load -> save) IDEMPOTENTE, que e o que o F4 precisa entre re-scans.
		if snapOut != "" {
			if err := e.SaveSnapshot(snapOut, nil, 0, 0); err != nil {
				fmt.Fprintf(os.Stderr, "[mftfile] aviso: snapshot: %v\n", err)
			}
		}
		if err := e.aggregate(); err != nil {
			fatal("agregar: " + err.Error())
		}
		rep := &Report{}
		e.fillReport(rep)
		if out == "" {
			out = filepath.Join(`E:\tools\maps\out\scans`, "loadsnap_"+time.Now().Format("20060102_150405"))
		}
		os.MkdirAll(out, 0o777)
		e.writeDirsCSV(filepath.Join(out, "dirs.csv"))
		writeJSON(filepath.Join(out, "meta.json"), rep)
		os.WriteFile(filepath.Join(out, "console.log"), []byte(renderConsole(rep, out)), 0o666)
		fmt.Fprintf(os.Stderr, "[mftfile] --load %s: arquivos=%d dirs=%d orfaos=%d bytes=%d\n",
			loadSnap, rep.Files, rep.Dirs, rep.Orphans, rep.Bytes)
		return
	}
	if len(paths) == 0 {
		fatal("uso: aetherscan mftfile <arquivo...> [--top N] [--out DIR] [--repeat N] [--parallel]")
	}
	if repeat < 1 {
		repeat = 1
	}
	if out == "" {
		ts := time.Now().Format("20060102_150405")
		base := strings.NewReplacer(`\`, "_", `:`, "", `/`, "_").Replace(paths[0])
		if len(base) > 60 {
			base = base[len(base)-60:]
		}
		out = filepath.Join(`E:\tools\maps\out\scans`, base+"_"+ts)
	}
	os.MkdirAll(out, 0o777)

	if len(paths) == 1 {
		path := paths[0]
		for it := 0; it < repeat; it++ {
			e := newMftEngine("IMG", top, false)
			e.started = time.Now()
			e.nobuf = noBuf // antes de openFile(): a flag vai no CreateFile
			if err := e.openFile(path); err != nil {
				fatal(err.Error())
			}
			e.applyIO(&scanOpts{IoRead: ioRead, NoBuf: noBuf})
			e.rootName = filepath.Base(path)
			// O snapshot e gravado dentro de build(), ANTES da agregacao
			// (o estado pos-agregacao nao e folha e re-agregar infla).
			e.snapDir = snapOut
			if err := e.build(); err != nil {
				fatal(err.Error())
			}
			rep := &Report{}
			tf := time.Now()
			e.fillReport(rep)
			if os.Getenv("AETHERSCAN_TIMING") != "" {
				fmt.Fprintf(os.Stderr, "[timing] fill_report    %7.3fs\n", time.Since(tf).Seconds())
			}
			fmt.Fprintf(os.Stderr, "[mftfile] iteracao %d/%d: %.4fs (arquivos=%d dirs=%d orfaos=%d recs=%d/%d)\n",
				it+1, repeat, rep.Duration, rep.Files, rep.Dirs, rep.Orphans, rep.RecsRead, rep.RecsTotal)
			if it == repeat-1 {
				finish(&scanOpts{Child: false}, out, rep, func(p string) error { return e.writeDirsCSV(p) })
			}
		}
		return
	}

	// ---- varios arquivos: pool (mesmo caminho do scan ALL --parallel) ----
	t0 := time.Now()
	jobs := make([]volJob, 0, len(paths))
	for _, p := range paths {
		p := p
		od := filepath.Join(out, strings.NewReplacer(`\`, "_", `:`, "", `/`, "_").Replace(filepath.Base(p)))
		os.MkdirAll(od, 0o777)
		jobs = append(jobs, volJob{name: filepath.Base(p), fn: func() (volRow, error) {
			e := newMftEngine("IMG", top, false)
			e.started = time.Now()
			e.nobuf = noBuf // antes de openFile(): a flag vai no CreateFile
			if err := e.openFile(p); err != nil {
				return volRow{}, err
			}
			e.applyIO(&scanOpts{IoRead: ioRead, NoBuf: noBuf})
			e.rootName = filepath.Base(p)
			if err := e.build(); err != nil {
				return volRow{}, err
			}
			rep := &Report{}
			e.fillReport(rep)
			txt := renderConsole(rep, od)
			os.WriteFile(filepath.Join(od, "console.log"), []byte(txt), 0o666)
			writeJSON(filepath.Join(od, "meta.json"), rep)
			e.writeDirsCSV(filepath.Join(od, "dirs.csv"))
			writeTopCSV(filepath.Join(od, "top_files.csv"), rep)
			return volRow{name: filepath.Base(p), files: rep.Files, dirs: rep.Dirs,
				bytes: rep.Bytes, alloc: rep.Alloc, secs: rep.Duration, orphans: rep.Orphans,
				recsR: int64(rep.RecsRead), recsT: rep.RecsTotal, extents: rep.MftSegs, console: txt}, nil
		}})
	}
	rows := runVolJobs(jobs, parallel, memBudget)
	fmt.Printf("\n%-40s %10s %14s %12s %8s\n", "ARQUIVO", "SEG", "ARQUIVOS", "BYTES", "ORFAOS")
	var soma float64
	for _, r := range rows {
		if r.name == "" {
			continue
		}
		soma += r.secs
		fmt.Printf("%-40s %8.2fs %14s %12s %8d\n", r.name, r.secs, fmtInt(r.files), fmtBytes(r.bytes), r.orphans)
	}
	fmt.Printf("\nparalelo=%v | soma dos tempos=%.2fs | WALL=%.2fs | ganho=%.2fx\n",
		parallel, soma, time.Since(t0).Seconds(), soma/time.Since(t0).Seconds())
}

// finish - grava meta.json / dirs.csv / top_files.csv / console.log (+ .done).
func finish(o *scanOpts, out string, rep *Report, dirsCSV func(string) error) {
	timing := os.Getenv("AETHERSCAN_TIMING") != ""
	t0 := time.Now()
	mark := func(name string) {
		if !timing {
			return
		}
		now := time.Now()
		fmt.Fprintf(os.Stderr, "[timing] %-14s %7.3fs\n", name, now.Sub(t0).Seconds())
		t0 = now
	}
	if err := writeJSON(filepath.Join(out, "meta.json"), rep); err != nil {
		fmt.Fprintln(os.Stderr, "aviso: meta.json:", err)
	}
	mark("meta_json")
	if dirsCSV != nil {
		if err := dirsCSV(filepath.Join(out, "dirs.csv")); err != nil {
			fmt.Fprintln(os.Stderr, "aviso: dirs.csv:", err)
		}
	}
	mark("dirs_csv")
	if err := writeTopCSV(filepath.Join(out, "top_files.csv"), rep); err != nil {
		fmt.Fprintln(os.Stderr, "aviso: top_files.csv:", err)
	}
	mark("top_files_csv")
	txt := renderConsole(rep, out)
	os.WriteFile(filepath.Join(out, "console.log"), []byte(txt), 0o666)
	fmt.Print(txt)
	mark("console")
	if o.Child {
		os.WriteFile(filepath.Join(out, ".done"), []byte("ok\n"), 0o666)
	}
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o666)
}

func renderConsole(r *Report, out string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== AetherScan | motor: %s | raiz: %s ===\n", r.Engine, r.Root)
	fmt.Fprintf(&b, "tempo: %.2fs | arquivos: %s | dirs: %s | reparse(skip): %s | erros: %s\n",
		r.Duration, fmtInt(r.Files), fmtInt(r.Dirs), fmtInt(r.Reparse), fmtInt(r.Errors))
	fmt.Fprintf(&b, "bytes(logico): %s | alocado: %s\n", fmtBytes(r.Bytes), fmtBytes(r.Alloc))
	if r.Engine == "mft" {
		fmt.Fprintf(&b, "[mft] deletados: %s | sem-nome: %s | orfaos: %s | registros-extensao: %s\n",
			fmtInt(r.Deleted), fmtInt(r.Unnamed), fmtInt(r.Orphans), fmtInt(r.ExtRecs))
		fmt.Fprintf(&b, "[mft] $MFT: extents=%d | runs=%d | data=%s | runlist=%s | registros lidos=%s/%s\n",
			r.MftSegs, r.MftRuns, fmtBytes(r.MftDataBytes), fmtBytes(r.MftRunBytes),
			fmtInt(int64(r.RecsRead)), fmtInt(int64(r.RecsTotal)))
	}
	fmt.Fprintf(&b, "\n-- TOP 30 DIRS --\n")
	for i, d := range r.TopDirs {
		if i >= 30 {
			break
		}
		fmt.Fprintf(&b, "  %12s  %s\n", fmtBytes(d.Size), d.Path)
	}
	fmt.Fprintf(&b, "\n-- TOP 25 FILES --\n")
	for i, f := range r.TopFiles {
		if i >= 25 {
			break
		}
		fmt.Fprintf(&b, "  %12s  %s\n", fmtBytes(f.Size), f.Path)
	}
	fmt.Fprintf(&b, "\n-- TOP 15 EXTENSOES --\n")
	for i, e := range r.Exts {
		if i >= 15 {
			break
		}
		fmt.Fprintf(&b, "  %12s  %-12s %s arqs\n", fmtBytes(e.Bytes), e.Ext, fmtInt(e.Count))
	}
	if len(r.ErrList) > 0 {
		fmt.Fprintf(&b, "\n-- ERROS (amostra) --\n")
		for i, e := range r.ErrList {
			if i >= 10 {
				break
			}
			fmt.Fprintf(&b, "  %s\n", e)
		}
	}
	fmt.Fprintf(&b, "\nsaída: %s\n", out)
	return b.String()
}

// waitChild - aguarda o processo elevado gravar .done.
func waitChild(out string) {
	marker := filepath.Join(out, ".done")
	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			if b, err := os.ReadFile(filepath.Join(out, "console.log")); err == nil {
				fmt.Println()
				fmt.Println(string(b))
			}
			fmt.Println("[mft] concluído. saída em:", out)
			return
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Println("timeout aguardando o processo elevado. verifique a janela elevada e a pasta:", out)
}

// mergeFileParts - concatena os shards files.partXX.csv em files.csv.
func mergeFileParts(out string) error {
	parts, err := filepath.Glob(filepath.Join(out, "files.part*.csv"))
	if err != nil || len(parts) == 0 {
		return err
	}
	dst, err := os.Create(filepath.Join(out, "files.csv"))
	if err != nil {
		return err
	}
	defer dst.Close()
	buf := make([]byte, 1<<20)
	for _, p := range parts {
		src, err := os.Open(p)
		if err != nil {
			continue
		}
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					src.Close()
					return werr
				}
			}
			if err != nil {
				break
			}
		}
		src.Close()
		os.Remove(p)
	}
	return nil
}
