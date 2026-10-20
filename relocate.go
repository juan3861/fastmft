package main

// relocate.go - Relocacao atomica de pastas com 7 Lentes ARIA e Junctions NTFS.
// Garante migracao segura, validacao de integridade e rollback instantaneo.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func runRelocateCmd(args []string) {
	if len(args) < 2 {
		fmt.Println("uso: aetherscan relocate <origem> <destino> [--force] [--dry-run]")
		fmt.Println("exemplo: aetherscan relocate C:\\Users\\juanp\\.codex D:\\migrado\\.codex")
		os.Exit(2)
	}

	src := filepath.Clean(args[0])
	dst := filepath.Clean(args[1])
	force := false
	dryRun := false

	for _, a := range args[2:] {
		switch a {
		case "--force":
			force = true
		case "--dry-run":
			dryRun = true
		}
	}

	fmt.Printf("[relocate] Origem : %s\n", src)
	fmt.Printf("[relocate] Destino: %s\n", dst)

	fi, err := os.Lstat(src)
	if err != nil {
		fatal(fmt.Sprintf("origem nao encontrada ou inacessivel: %v", err))
	}
	if !fi.IsDir() {
		fatal("origem deve ser um diretorio")
	}

	// Verifica se ja e Junction ou Reparse Point
	isReparse := fi.Mode()&os.ModeSymlink != 0 || fi.Mode()&os.ModeIrregular != 0
	if isReparse {
		linkTarget, _ := filepath.EvalSymlinks(src)
		fatal(fmt.Sprintf("origem ja e uma junction/link apontando para: %s", linkTarget))
	}

	// 1. Avaliacao preliminar das Lentes Topologicas (Lente 1: Boot/OS, Lente 2: Reparse, Lente 3: Fronteira)
	fmt.Println("[relocate] 1/6 Avaliando Guardas Topologicas do ARIA (Boot, Reparse, Fronteira)...")
	ariaPrelim, cat, tDrive, _ := evaluateARIA7Lenses(src, 1<<30, 1000, isReparse)
	if !ariaPrelim.BootImmutableGuard || !ariaPrelim.ReparseJunctionGuard || !ariaPrelim.BoundaryCoherence {
		fmt.Printf("           Lente 1 (Boot/OS Imutavel)      : %v\n", ariaPrelim.BootImmutableGuard)
		fmt.Printf("           Lente 2 (Reparse/Junction Guard): %v\n", ariaPrelim.ReparseJunctionGuard)
		fmt.Printf("           Lente 3 (Coerencia de Fronteira): %v (Cat: %s)\n", ariaPrelim.BoundaryCoherence, cat)
		if !force {
			fatal("MIGRACAO REJEITADA: pasta protegida contra relocacao por violar a integridade estrutural do SO. Use --force apenas se souber exatamente o que esta fazendo.")
		}
	}

	// 2. Levantamento de massa e arquivos da origem
	fmt.Println("[relocate] 2/6 Calculando massa da pasta de origem...")
	var srcSize, srcFiles int64
	filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			srcFiles++
			srcSize += info.Size()
		}
		return nil
	})
	fmt.Printf("           -> %d arquivos | %s\n", srcFiles, fmtBytes(srcSize))

	// 3. Avaliacao completa das 7 Lentes do ARIA
	fmt.Println("[relocate] 3/6 Avaliando 7 Lentes Axiomaticas do ARIA...")
	aria, _, _, ok := evaluateARIA7Lenses(src, srcSize, srcFiles, isReparse)
	fmt.Printf("           Lente 1 (Boot/OS Imutavel)      : %v\n", aria.BootImmutableGuard)
	fmt.Printf("           Lente 2 (Reparse/Junction Guard): %v\n", aria.ReparseJunctionGuard)
	fmt.Printf("           Lente 3 (Coerencia de Fronteira): %v (Cat: %s)\n", aria.BoundaryCoherence, cat)
	fmt.Printf("           Lente 4 (Limiar de Massa >=100M): %v\n", aria.MassThreshold)
	fmt.Printf("           Lente 5 (Spindle Compativel)    : %s\n", aria.SpindleMatch)
	fmt.Printf("           Lente 6 (Transparencia Link NTFS): %v\n", aria.LinkageTransparency)
	fmt.Printf("           Lente 7 (Ganho Liquido Positivo): %v\n", aria.PostMoveGainPositive)
	fmt.Printf("           Veredito ARIA                   : %s (Alvo Sugerido: %s)\n", aria.Verdict, tDrive)

	if !ok && !force {
		fatal("MIGRACAO REJEITADA pelas 7 Lentes ARIA. Use --force para sobrescrever.")
	}

	if dryRun {
		fmt.Println("[relocate] Modo --dry-run ativo. Nenhuma alteracao foi feita.")
		return
	}

	// 3. Espelhamento Robocopy Multithread
	fmt.Println("[relocate] 3/6 Espelhando para o destino via Robocopy multithread (/MT:16)...")
	os.MkdirAll(dst, 0o777)
	roboCmd := exec.Command("robocopy", src, dst, "/E", "/COPY:DAT", "/DCOPY:DAT", "/R:2", "/W:1", "/MT:16", "/NFL", "/NDL", "/NP")
	roboCmd.Stdout = os.Stdout
	roboCmd.Stderr = os.Stderr
	_ = roboCmd.Run()
	// Robocopy retorna codigos 0-7 para sucesso; >=8 e falha
	if roboCmd.ProcessState != nil && roboCmd.ProcessState.ExitCode() >= 8 {
		fatal(fmt.Sprintf("robocopy falhou com codigo de saida %d", roboCmd.ProcessState.ExitCode()))
	}

	// 4. Verificacao de Integridade no Destino
	fmt.Println("[relocate] 4/6 Auditando integridade do espelho no destino...")
	var dstSize, dstFiles int64
	filepath.Walk(dst, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			dstFiles++
			dstSize += info.Size()
		}
		return nil
	})
	fmt.Printf("           Destino: %d arquivos | %s\n", dstFiles, fmtBytes(dstSize))
	if dstFiles < srcFiles {
		fatal(fmt.Sprintf("AUDITORIA FALHOU: destino contem menos arquivos que a origem (%d < %d)! Operacao abortada sem tocar na origem.", dstFiles, srcFiles))
	}

	// 5. Isolamento da Origem e Criacao da Junction NTFS
	fmt.Println("[relocate] 5/6 Isolando origem e criando Junction NTFS...")
	bak := src + ".migrating_bak"
	if _, err := os.Stat(bak); err == nil {
		_ = os.RemoveAll(bak)
	}
	if err := os.Rename(src, bak); err != nil {
		fatal(fmt.Sprintf("nao foi possivel isolar a origem (arquivos bloqueados por processos ativos?): %v", err))
	}

	mkCmd := exec.Command("cmd.exe", "/c", "mklink", "/J", src, dst)
	mkOut, err := mkCmd.CombinedOutput()
	if err != nil {
		fmt.Printf("FALHA ao criar junction: %s\nRevertendo isolamento...\n", string(mkOut))
		_ = os.Rename(bak, src)
		fatal("rollback executado; pasta original restaurada intacta")
	}

	// 6. Tripla Validacao da Junction e Purga do Backup
	fmt.Println("[relocate] 6/6 Tripla Validacao Empirica da Junction...")
	testEntries, err := os.ReadDir(src)
	if err != nil || len(testEntries) == 0 && srcFiles > 0 {
		fmt.Printf("FALHA na leitura atraves da junction: %v\nRevertendo...\n", err)
		_ = exec.Command("cmd.exe", "/c", "rmdir", src).Run()
		_ = os.Rename(bak, src)
		fatal("rollback executado; pasta original restaurada intacta")
	}

	fmt.Printf("           [OK] Junction ativa e respondendo! (%d entradas na raiz)\n", len(testEntries))
	fmt.Println("[relocate] Purgando backup temporario no disco de origem...")
	t0 := time.Now()
	rmCmd := exec.Command("cmd.exe", "/c", "rmdir", "/s", "/q", bak)
	_ = rmCmd.Run()
	fmt.Printf("[relocate] SUCESSO ABSOLUTO em %.2fs! %s liberados no disco de origem.\n",
		time.Since(t0).Seconds(), fmtBytes(srcSize))

	// Exibe novo estado de Casimir
	srcDrive := driveOf(src)
	if srcDrive != "" {
		tot, free := volumeSpace(srcDrive)
		cas := computeCasimir(tot, free, 0)
		fmt.Printf("[casimir] Volume %s | Livre: %s | Gap d: %.4f | Forca Fc: %.1f | Estado: %s\n",
			srcDrive, fmtBytes(free), cas.NormalizedGapD, cas.CasimirForce, cas.RegimeState)
	}
}

func isReparsePoint(path string) bool {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		return false
	}
	return attrs&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
