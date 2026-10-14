package main

// selftest.go - fuzzing IN-PROCESS do parser MFT (F5c).
//
// Motivo: os 6 bugs encontrados no motor eram TODOS silenciosos — emitiam
// numeros plausiveis. Um parser de formato binario precisa provar mais que
// "acerta o corpus feliz": o disco real tem setor rasgado, MFT truncada,
// registro parcialmente sobrescrito, fixup invalido. O parser NAO pode entrar
// em panico nem travar.
//
// Aqui a entrada e mutada em memoria e o motor roda contra um readFn de memoria
// (sem tocar o disco), entao milhares de casos rodam em segundos.
//
// Uso: aetherscan selftest --fuzz <corpus.bin> [--iters N] [--seed S] [--max-mut K]

import (
	"fmt"
	"math/rand"
	"os"
	"time"
)

type fuzzStats struct {
	iters     int
	panics    int
	orphanSum int64
	maxOrph   int64
	zeroFiles int
}

func runSelfTest(args []string) {
	// Gate F4 (incremental via USN Journal) tem entrada propria.
	for i, a := range args {
		if a == "--f4" {
			rest := append([]string{}, args[:i]...)
			rest = append(rest, args[i+1:]...)
			runF4Gate(rest)
			return
		}
		if a == "--iosweep" {
			rest := append([]string{}, args[:i]...)
			rest = append(rest, args[i+1:]...)
			runIOSweep(rest)
			return
		}
	}
	var corpus string
	iters := 200
	seed := int64(1)
	maxMut := 6
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--fuzz" && i+1 < len(args):
			corpus = args[i+1]
			i += 2
		case a == "--iters" && i+1 < len(args):
			iters = atoiSafe(args[i+1], 200)
			i += 2
		case a == "--seed" && i+1 < len(args):
			seed = int64(atoiSafe(args[i+1], 1))
			i += 2
		case a == "--max-mut" && i+1 < len(args):
			maxMut = atoiSafe(args[i+1], 6)
			i += 2
		case !startsWith(a, "-"):
			corpus = a
			i++
		default:
			fatal("flag desconhecida: " + a)
		}
	}
	if corpus == "" {
		fatal("uso: aetherscan selftest --fuzz <corpus.bin> [--iters N] [--seed S]")
	}
	data, err := os.ReadFile(corpus)
	if err != nil {
		fatal(err.Error())
	}
	if len(data) < 4096 {
		fatal("corpus pequeno demais")
	}

	base := len(data)
	rng := rand.New(rand.NewSource(seed))
	var st fuzzStats
	t0 := time.Now()

	for it := 0; it < iters; it++ {
		mut := make([]byte, base)
		copy(mut, data)

		nMut := 1 + rng.Intn(maxMut)
		for m := 0; m < nMut; m++ {
			applyMutation(mut, rng)
		}
		// metade dos casos testa truncamento (MFT cortada no meio)
		if it%2 == 1 && len(mut) > 8192 {
			cut := 4096 + rng.Intn(len(mut)-4096)
			mut = mut[:cut]
		}

		st.iters++
		func() {
			defer func() {
				if r := recover(); r != nil {
					st.panics++
					if st.panics <= 5 {
						fmt.Fprintf(os.Stderr, "[fuzz] PANICO na iteracao %d (seed=%d): %v\n", it, seed, r)
					}
				}
			}()
			orph, files := fuzzOnce(mut)
			st.orphanSum += orph
			if orph > st.maxOrph {
				st.maxOrph = orph
			}
			if files == 0 {
				st.zeroFiles++
			}
		}()
	}

	dt := time.Since(t0).Seconds()
	fmt.Printf("=== selftest (fuzz) ===\n")
	fmt.Printf("corpus: %s (%.1f MB, %d registros)\n", corpus, float64(base)/(1<<20), base/1024)
	fmt.Printf("iteracoes: %d | seed: %d | mutacoes/caso: 1..%d | tempo: %.2fs (%.0f casos/s)\n",
		st.iters, seed, maxMut, dt, float64(st.iters)/dt)
	fmt.Printf("panicos: %d | orfaos: max=%d media=%.1f | casos com 0 arquivos: %d\n",
		st.panics, st.maxOrph, float64(st.orphanSum)/float64(st.iters), st.zeroFiles)

	if st.panics > 0 {
		fmt.Println("RESULTADO: FALHOU (houve panico)")
		os.Exit(1)
	}
	fmt.Println("RESULTADO: OK (nenhum panico, todas as entradas terminaram)")
}

// fuzzOnce - roda o motor contra os bytes dados (sem disco). Devolve (orfaos, arquivos).
func fuzzOnce(mut []byte) (int64, int64) {
	e := newMftEngine("FUZZ", 50, false)
	e.rawMft = true
	e.bytesPerSector = 512
	e.clusterSize = 4096
	e.recordSize = 1024
	e.mftStart = 0
	e.recordCount = len(mut) / e.recordSize
	e.started = time.Now()

	dat := mut
	e.readFn = func(off int64, buf []byte) error {
		if off < 0 || off >= int64(len(dat)) {
			for i := range buf {
				buf[i] = 0
			}
			return nil
		}
		n := copy(buf, dat[off:])
		for i := n; i < len(buf); i++ {
			buf[i] = 0
		}
		return nil
	}
	if e.recordCount < 16 {
		return 0, 0
	}
	if err := e.build(); err != nil {
		return 0, 0
	}
	return e.orphans, int64(e.filesAgg[rootRecIdx])
}

// applyMutation - aplica UMA mutacao aleatoria aos bytes (simula corrupcao real).
//
// As mutacoes sao enviesadas para a ARVORE (registros >= 16). Corromper sempre o
// registro 0 ($MFT) ou os metadados faz o motor sair cedo e o fuzz cobre pouco;
// atingindo a arvore, o parser percorre de verdade os caminhos de $FILE_NAME,
// $DATA, registros de extensao e reconstrucao de hierarquia.
func applyMutation(b []byte, rng *rand.Rand) {
	if len(b) < 16*1024 {
		return
	}
	nrec := len(b) / 1024
	pickRec := func() int {
		if rng.Intn(10) == 0 {
			return rng.Intn(16) * 1024 // 10%: metadados/raiz
		}
		return (16 + rng.Intn(nrec-16)) * 1024 // 90%: arvore
	}
	switch rng.Intn(9) {
	case 0: // flip de byte aleatorio na arvore
		b[pickRec()+rng.Intn(1024)] ^= byte(1 << uint(rng.Intn(8)))
	case 1: // zera um bloco alinhado a 512
		off := rng.Intn(len(b)/512) * 512
		for i := 0; i < 512 && off+i < len(b); i++ {
			b[off+i] = 0
		}
	case 2: // corrompe a Update Sequence Array de um registro
		rec := pickRec()
		usaOff := int(uint16(b[rec+4]) | uint16(b[rec+5])<<8)
		if usaOff > 0 && rec+usaOff+2 <= len(b) {
			b[rec+usaOff] ^= 0xFF
		}
	case 3: // corrompe o attrsOff (aponta p/ dentro do cabecalho)
		rec := pickRec()
		v := uint16(rng.Intn(64))
		b[rec+20] = byte(v)
		b[rec+21] = byte(v >> 8)
	case 4: // flags do registro (dir/em-uso aleatorios)
		rec := pickRec()
		b[rec+22] = byte(rng.Intn(256))
	case 5: // byte no meio dos atributos (runlist, $FILE_NAME, $DATA)
		rec := pickRec()
		b[rec+56+rng.Intn(900)] = byte(rng.Intn(256))
	case 6: // copia um registro sobre outro (parent/baseRef circular)
		dst := pickRec()
		src := pickRec()
		copy(b[dst:dst+1024], b[src:src+1024])
	case 7: // destroi o magic
		rec := pickRec()
		b[rec] = byte(rng.Intn(256))
	case 8: // corrompe o parent-ref do $FILE_NAME (ciclo na hierarquia)
		rec := pickRec()
		ao := int(uint16(b[rec+20]) | uint16(b[rec+21])<<8)
		for off := ao; off+24 < 1024; {
			t := uint32(b[rec+off]) | uint32(b[rec+off+1])<<8 | uint32(b[rec+off+2])<<16 | uint32(b[rec+off+3])<<24
			l := int(uint32(b[rec+off+4]) | uint32(b[rec+off+5])<<8 | uint32(b[rec+off+6])<<16 | uint32(b[rec+off+7])<<24)
			if t == 0xFFFFFFFF || l < 16 || off+l > 1024 {
				break
			}
			if t == 0x30 {
				vo := int(uint16(b[rec+off+20]) | uint16(b[rec+off+21])<<8)
				base := rec + off + vo
				if base+8 <= rec+1024 {
					for k := 0; k < 8; k++ {
						b[base+k] = byte(rng.Intn(256))
					}
				}
				break
			}
			off += l
		}
	}
}

func startsWith(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}
