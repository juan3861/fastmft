package main

// iosweep.go - F1 reformulado: por que o motor extrai 178 MB/s de um volume cru
// que o instrumento mede em 270 MB/s?
//
// O rawprobe (instrumento independente) mostrou que o VOLUME CRU do C: entrega
// 265-294 MB/s com profundidade de fila 0..8 e blocos de 8/32 MB - e que a
// profundidade NAO importa (<8%). Logo o gap de 178 MB/s nao e fila.
//
// Suspeito principal: TAMANHO DE BLOCO. O motor le 4096 registros = 4 MB por vez
// por default. Este comando roda o motor REAL no volume cru com varios valores de
// --ioread e reporta MB/s de cada um, no mesmo processo e no mesmo regime - que e
// o unico jeito de comparar sem contaminacao entre execucoes.
//
// Uso: aetherscan selftest --iosweep C: [--ioreads 4,8,16,32] [--elevate]

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func runIOSweep(args []string) {
	drive := "C:"
	elev := false
	vals := []int{4, 8, 16, 32}
	bufmbs := []int{}
	workerss := []int{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--elevate":
			elev = true
		case a == "--timing":
			os.Setenv("AETHERSCAN_TIMING", "1")
		case a == "--iostats":
			os.Setenv("AETHERSCAN_IOSTATS", "1")
		case a == "--workerss" && i+1 < len(args):
			i++
			workerss = parseIOCSV(args[i])
		case strings.HasPrefix(a, "--workerss="):
			workerss = parseIOCSV(strings.TrimPrefix(a, "--workerss="))
		case a == "--bufmbs" && i+1 < len(args):
			i++
			bufmbs = parseIOCSV(args[i])
		case strings.HasPrefix(a, "--bufmbs="):
			bufmbs = parseIOCSV(strings.TrimPrefix(a, "--bufmbs="))
		case a == "--ioreads" && i+1 < len(args):
			i++
			vals = parseIOCSV(args[i])
		case strings.HasPrefix(a, "--ioreads="):
			vals = parseIOCSV(strings.TrimPrefix(a, "--ioreads="))
		case !strings.HasPrefix(a, "-"):
			drive = strings.ToUpper(strings.TrimSuffix(a, "\\"))
			if !strings.HasSuffix(drive, ":") {
				drive += ":"
			}
		}
	}

	if !isElevated() {
		if !elev {
			fatal("o iosweep requer administrador (abre o volume cru).\n  use: --elevate")
		}
		csv := make([]string, len(vals))
		for i, v := range vals {
			csv[i] = strconv.Itoa(v)
		}
		fmt.Println("[iosweep] disparando processo elevado (UAC)... confirme o prompt do Windows.")
		child := []string{"selftest", "--iosweep", drive, "--ioreads", strings.Join(csv, ",")}
		// o filho elevado e um processo NOVO: flags nao se propagam sozinhas.
		// (--timing ficou de fora na 1a versao e o log saiu sem as fases.)
		if os.Getenv("AETHERSCAN_TIMING") != "" {
			child = append(child, "--timing")
		}
		if len(workerss) > 0 {
			wc := make([]string, len(workerss))
			for i, v := range workerss {
				wc[i] = strconv.Itoa(v)
			}
			child = append(child, "--workerss", strings.Join(wc, ","))
		}
		if len(bufmbs) > 0 {
			bc := make([]string, len(bufmbs))
			for i, v := range bufmbs {
				bc[i] = strconv.Itoa(v)
			}
			child = append(child, "--bufmbs", strings.Join(bc, ","))
		}
		if err := elevateSelf(child); err != nil {
			fatal("falha ao elevar: " + err.Error())
		}
		return
	}

	reps := 3
	_ = os.Getenv("AETHERSCAN_TIMING")
	root := `E:\tools\maps\out\iosweep`
	os.MkdirAll(root, 0o777)
	if lf, err := os.Create(filepath.Join(root, "iosweep.log")); err == nil {
		os.Stdout = lf
		os.Stderr = lf
		defer lf.Close()
	}

	fmt.Printf("=== IOSWEEP (INTERCALADO) | drive=%s | ioreads=%v | reps=%d ===\n", drive, vals, reps)
	fmt.Printf("Protocolo: A-B-A-B..., SEMPRE com --nobuf (leitura sem cache). Rodar cada\n")
	fmt.Printf("config em sequencia faz a 2a. herdar o cache da 1a. - foi o que produziu um\n")
	fmt.Printf("falso +18%% na primeira versao deste sweep.\n\n")
	type cfg struct {
		label string
		io    int
		buf   int
		wrk   int
	}
	var cfgs []cfg
	if len(workerss) > 0 {
		for _, w := range workerss {
			cfgs = append(cfgs, cfg{fmt.Sprintf("workers=%d", w), vals[0], 0, w})
		}
	} else if len(bufmbs) > 0 {
		for _, b := range bufmbs {
			cfgs = append(cfgs, cfg{fmt.Sprintf("buf=%dMB", b), vals[0], b, 0})
		}
	} else {
		for _, io := range vals {
			cfgs = append(cfgs, cfg{fmt.Sprintf("ioread=%dMB", io), io, 0, 0})
		}
	}
	samples := map[string][]float64{}
	for r := 0; r < reps; r++ {
		for _, c := range cfgs {
			ioStatsReset()
			e := newMftEngine(drive, 0, false)
			e.started = time.Now()
			e.nobuf = true // SEM cache: e o unico modo comparavel entre rodadas
			// grava o dump da runlist (mft_runs.txt) p/ diagnostico de fragmentacao
			if r == 0 && len(bufmbs) == 0 && len(workerss) == 0 {
				e.outDir = `E:\tools\maps\out\iosweep`
			}
			if err := e.open(); err != nil {
				fmt.Printf("rep%d %-14s erro: %v\n", r+1, c.label, err)
				continue
			}
			e.applyIO(&scanOpts{IoRead: c.io, BufMB: c.buf, Workers: c.wrk, NoBuf: true})
			if err := e.build(); err != nil {
				fmt.Printf("rep%d %-14s erro: %v\n", r+1, c.label, err)
				closeHandle(e.h)
				continue
			}
			rep := &Report{}
			e.fillReport(rep)
			closeHandle(e.h)
			mb := float64(e.mftDataRaw) / 1e6
			mbps := 0.0
			if rep.Duration > 0 {
				mbps = mb / rep.Duration
			}
			samples[c.label] = append(samples[c.label], mbps)
			fmt.Printf("rep%d %-14s %8.2fs %8.1f MB/s\n", r+1, c.label, rep.Duration, mbps)
			if st := ioStatsLine(); st != "" {
				fmt.Printf("       %s\n", st)
			}
		}
	}
	fmt.Printf("\n%-16s %8s %8s %8s %8s\n", "config", "min", "mediana", "max", "n")
	for _, c := range cfgs {
		v := samples[c.label]
		if len(v) == 0 {
			continue
		}
		sort.Float64s(v)
		fmt.Printf("%-16s %8.1f %8.1f %8.1f %8d\n", c.label, v[0], v[len(v)/2], v[len(v)-1], len(v))
	}
	fmt.Printf("logs: %s\n", filepath.Join(root, "iosweep.log"))
}

// parseIOCSV - "4,8,16,32" -> []int
func parseIOCSV(s string) []int {
	out := []int{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		out = []int{4, 8, 16, 32}
	}
	return out
}
