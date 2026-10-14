package main

// pool.go - execucao de VARIOS volumes em paralelo.
//
// Por que existe: no fluxo "mapear todos os discos", o runMftAll original
// processa os volumes EM SERIE. Como C:/F: e D:/E: sao discos fisicos
// diferentes, o tempo total vira a SOMA dos tempos. Executando em paralelo o
// total passa a ser ~MAX: o I/O de spindles distintos se sobrepoe.
// Medido: 3,0x no caso quente e 17,6x quando um volume estava frio.
//
// O RISCO e RAM: cada motor mantem arrays proporcionais ao nº de registros.
// Medido com o motor atual: 452 MB para 3,04 M registros (156 B/registro); o F:
// tem 8 M -> ~1,25 GB. Quatro volumes simultaneos passariam de 2,5 GB. Por isso
// a concorrencia e limitada por um ORCAMENTO DE RAM (60% da livre por padrao,
// ajustavel com --mem-budget), e nao pelo numero de volumes.

import (
	"fmt"
	"os"
	"sync"
)

// volRow - linha de resultado de um volume.
type volRow struct {
	name           string
	files, dirs    int64
	bytes, alloc   int64
	secs           float64
	orphans, recsR int64
	recsT, extents int
	console        string // saída legível (impressa depois, em ordem)
}

// volJob - trabalho de UM volume.
type volJob struct {
	name string
	fn   func() (volRow, error)
}

// perVolBytes - estimativa conservadora de RAM por volume. Medido: 452 MB para
// 3,04 M registros; F: (8 M registros) fica em ~1,25 GB. 700 MB cobre a maioria
// e o teto de concorrencia impede o estouro quando um volume grande entra junto.
const perVolBytes = 700 << 20

// runVolJobs - executa os jobs sequencialmente ou em paralelo (limitado por RAM).
func runVolJobs(jobs []volJob, parallel bool, memBudgetMB int) []volRow {
	rows := make([]volRow, len(jobs))
	if !parallel || len(jobs) < 2 {
		for i, j := range jobs {
			r, err := j.fn()
			if err != nil {
				fmt.Fprintf(os.Stderr, "[mft] %s: %v\n", j.name, err)
				continue
			}
			rows[i] = r
		}
		return rows
	}

	avail := int64(availableRAM())
	var budget int64
	if memBudgetMB > 0 {
		budget = int64(memBudgetMB) << 20
	} else {
		budget = int64(float64(avail) * 0.6)
	}
	conc := int(budget / perVolBytes)
	if conc < 1 {
		conc = 1
	}
	if conc > len(jobs) {
		conc = len(jobs)
	}
	fmt.Printf("[mft] modo PARALELO: %d volumes | concorrencia=%d (RAM livre=%.1f GB, orcamento=%.1f GB, ~%.0f MB/volume)\n",
		len(jobs), conc, float64(avail)/(1<<30), float64(budget)/(1<<30), float64(perVolBytes)/(1<<20))
	if conc == 1 {
		fmt.Println("[mft] AVISO: RAM livre baixa -> rodando 1 volume por vez (use --mem-budget para forcar)")
	}

	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, err := jobs[i].fn()
			if err != nil {
				fmt.Fprintf(os.Stderr, "[mft] %s: %v\n", jobs[i].name, err)
				return
			}
			rows[i] = r
		}(i)
	}
	wg.Wait()
	return rows
}
