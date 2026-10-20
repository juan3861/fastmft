package main

// projects.go - Radar Instantaneo de Projetos de Software e Limpeza de Parasitas.
// Mapeia linguagens, repositorios git e calcula a massa de build artifacts/dependencias.

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ProjectDetail struct {
	Path           string            `json:"path"`
	Name           string            `json:"name"`
	Type           string            `json:"type"`
	Markers        []string          `json:"markers"`
	TotalSizeBytes int64             `json:"total_size_bytes"`
	TotalFiles     int64             `json:"total_files"`
	ParasiteBytes  int64             `json:"parasite_bytes"`
	ParasiteFiles  int64             `json:"parasite_files"`
	SourceBytes    int64             `json:"source_bytes"`
	WastePercent   float64           `json:"waste_percent"`
	Parasites      []ParasiteSummary `json:"parasites"`
}

type ParasiteSummary struct {
	Category  string `json:"category"`
	RelPath   string `json:"relative_path"`
	SizeBytes int64  `json:"size_bytes"`
	Files     int64  `json:"files"`
}

var projectMarkerMap = map[string]string{
	".git":             "Git Repo",
	"cargo.toml":       "Rust",
	"go.mod":           "Go",
	"package.json":     "Node/JS",
	"pnpm-lock.yaml":   "Node/pnpm",
	"pyproject.toml":   "Python",
	"requirements.txt": "Python",
	"cmakelists.txt":   "C/C++",
	"pom.xml":          "Java/Maven",
	"build.gradle":     "Java/Gradle",
}

var parasiteDirNames = map[string]string{
	"node_modules":   "Node Dependencies",
	"target":         "Rust/Cargo Build",
	".venv":          "Python Venv",
	"venv":           "Python Venv",
	"__pycache__":    "Python Bytecode",
	".pytest_cache":  "Pytest Cache",
	".next":          "Next.js Build",
	".nuxt":          "Nuxt.js Build",
	"dist":           "Build Distribution",
	"build":          "Compiled Build",
	".turbo":         "Turborepo Cache",
	".gradle":        "Gradle Cache",
	"bin":            "Dotnet Bin",
	"obj":            "Dotnet Obj",
}

func runProjectsCmd(args []string) {
	target := "C:\\Users\\juanp"
	top := 50
	filterParasites := false

	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--top" && i+1 < len(args):
			top = atoiSafe(args[i+1], 50)
			i += 2
		case a == "--parasites":
			filterParasites = true
			i++
		case !strings.HasPrefix(a, "-"):
			target = a
			i++
		default:
			i++
		}
	}
	target = normalizeRoot(target)

	fmt.Printf("[projects] Mapeando projetos em: %s (Top %d)...\n", target, top)
	t0 := time.Now()

	w := newWalkers(target, 0, top*2, false, nil)
	w.run()
	dur := time.Since(t0).Seconds()

	n := w.arena.len()
	paths := make([]string, n)
	if n > 0 {
		paths[0] = w.arena.at(0).Name
	}
	for idx := 1; idx < n; idx++ {
		nd := w.arena.at(int32(idx))
		paths[idx] = joinPath(paths[nd.Parent], nd.Name)
	}

	// 1. Identifica os nós raízes de projetos
	isProjRoot := make([]bool, n)
	projMarkers := make([][]string, n)

	for idx := 1; idx < n; idx++ {
		nd := w.arena.at(int32(idx))
		lowName := strings.ToLower(nd.Name)
		if tag, ok := projectMarkerMap[lowName]; ok && nd.Parent >= 0 {
			pIdx := nd.Parent
			isProjRoot[pIdx] = true
			projMarkers[pIdx] = append(projMarkers[pIdx], tag)
		}
	}

	// 2. Mapeia parasitas para a raiz de projeto ancestral mais proxima
	projParasites := make([][]ParasiteSummary, n)
	for idx := 1; idx < n; idx++ {
		nd := w.arena.at(int32(idx))
		subName := strings.ToLower(nd.Name)
		if cat, isPara := parasiteDirNames[subName]; isPara {
			curr := nd.Parent
			for curr >= 0 {
				if isProjRoot[curr] {
					rel, _ := filepath.Rel(paths[curr], paths[idx])
					projParasites[curr] = append(projParasites[curr], ParasiteSummary{
						Category:  cat,
						RelPath:   rel,
						SizeBytes: nd.Size,
						Files:     nd.Files,
					})
					break
				}
				if curr == 0 {
					break
				}
				curr = w.arena.at(curr).Parent
			}
		}
	}

	// 3. Consolida metricas de cada projeto
	var projects []ProjectDetail
	var totalWaste int64

	for idx := 0; idx < n; idx++ {
		if !isProjRoot[idx] {
			continue
		}
		pPath := paths[idx]
		lowP := strings.ToLower(pPath)
		// Ignora projetos aninhados dentro de node_modules ou .git
		if strings.Contains(lowP, `\node_modules\`) || strings.Contains(lowP, `\.git\`) {
			continue
		}

		nd := w.arena.at(int32(idx))
		markers := projMarkers[idx]
		projType := "Generic Project"
		if len(markers) > 0 {
			projType = markers[0]
		}

		parasites := projParasites[idx]
		var pBytes, pFiles int64
		for _, pa := range parasites {
			pBytes += pa.SizeBytes
			pFiles += pa.Files
		}

		sourceBytes := nd.Size - pBytes
		if sourceBytes < 0 {
			sourceBytes = 0
		}
		wastePct := 0.0
		if nd.Size > 0 {
			wastePct = math.Round((float64(pBytes)/float64(nd.Size))*1000) / 10.0
		}

		totalWaste += pBytes

		projects = append(projects, ProjectDetail{
			Path:           pPath,
			Name:           filepath.Base(pPath),
			Type:           projType,
			Markers:        markers,
			TotalSizeBytes: nd.Size,
			TotalFiles:     nd.Files,
			ParasiteBytes:  pBytes,
			ParasiteFiles:  pFiles,
			SourceBytes:    sourceBytes,
			WastePercent:   wastePct,
			Parasites:      parasites,
		})
	}

	// Ordena por maior tamanho total ou maior desperdício
	if filterParasites {
		sort.Slice(projects, func(a, b int) bool {
			return projects[a].ParasiteBytes > projects[b].ParasiteBytes
		})
	} else {
		sort.Slice(projects, func(a, b int) bool {
			return projects[a].TotalSizeBytes > projects[b].TotalSizeBytes
		})
	}

	if len(projects) > top {
		projects = projects[:top]
	}

	// Exibe Relatorio Formatado
	fmt.Printf("\n[projects] Varredura concluida em %.2fs. Total de Projetos Localizados: %d\n", dur, len(projects))
	fmt.Printf("[projects] Desperdicio Total de Build/Dependencias: %s\n\n", fmtBytes(totalWaste))

	fmt.Println("----------------------------------------------------------------------------------------------------------------")
	fmt.Printf("%-4s | %-15s | %-12s | %-10s | %-10s | %-8s | %s\n",
		"#", "TIPO", "TOTAL", "CODIGO", "PARASITAS", "% LIXO", "CAMINHO DO PROJETO")
	fmt.Println("----------------------------------------------------------------------------------------------------------------")

	for k, pr := range projects {
		fmt.Printf("%-4d | %-15s | %-12s | %-10s | %-10s | %6.1f%% | %s\n",
			k+1,
			truncateStr(pr.Type, 15),
			fmtBytes(pr.TotalSizeBytes),
			fmtBytes(pr.SourceBytes),
			fmtBytes(pr.ParasiteBytes),
			pr.WastePercent,
			pr.Path,
		)
		if len(pr.Parasites) > 0 && filterParasites {
			for _, pa := range pr.Parasites {
				fmt.Printf("       -> [%s] %s: %s (%d arqs)\n",
					pa.Category, pa.RelPath, fmtBytes(pa.SizeBytes), pa.Files)
			}
		}
	}
	fmt.Println("----------------------------------------------------------------------------------------------------------------")
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-2] + ".."
}
