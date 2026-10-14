package main

// winapi.go - wrappers diretos para kernel32/shell32 via LazyDLL.
// Zero dependencias externas: apenas stdlib do Go.
// Pecas extraidas dos campeoes:
//   - gdu/diskus: enumeracao em lote + parallelismo
//   - WizTree/Everything (conceito): leitura direta da MFT
// Aqui ficam as chamadas de mais baixo nivel.

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Contadores de I/O do caminho de volume cru. Diagnostico F1: o motor mede
// ~216 MB/s onde o instrumento independente mede 257-262 MB/s nos MESMOS bytes.
// So ha um jeito de descobrir o motivo: contar o que esta sendo pedido e o que
// volta de cada ReadFile.
var (
	ioStatCalls    int64 // nº de chamadas ReadFile
	ioStatBytesReq int64 // bytes pedidos (somatorio)
	ioStatBytesGot int64 // bytes efetivamente lidos
	ioStatShort    int64 // leituras que voltaram MENORES que o pedido
	ioStatErr      int64 // ReadFile que falharam
	ioStatIONs     int64 // tempo acumulado DENTRO do ReadFile (ns)

	// Separados por tamanho do pedido: o stream grande e as leituras de
	// METADADOS ($ATTRIBUTE_LIST, boot, registros individuais) que se
	// intercalam nele. Hipótese F1: as pequenas forçam o dispositivo a
	// reposicionar e derrubam a vazão do stream grande.
	ioStatBigCalls    int64
	ioStatSmallCalls  int64
	ioStatBigNs       int64
	ioStatSmallNs     int64
	ioStatBigBytesReq int64
	ioStatSmallBytesR int64

	// Log dos primeiros pedidos FORA do stream: offset+tamanho identificam qual
	// caminho do motor os emite (diagnostico F1).
	ioOutN int
	ioOutB [60][3]int64

	ioHistCnt [9]int64
	ioHistNs  [9]int64
)

// ioHistBnd - limites dos buckets do histograma (ultimo = infinito).
var ioHistBnd = [9]int64{0, 4096, 65536, 1 << 20, 4 << 20, 8 << 20, 16 << 20, 32 << 20, 1 << 62}

var ioHistName = [9]string{"<4KB", "4-64KB", "64KB-1MB", "1-4MB", "4-8MB", "8-16MB", "16-32MB", ">=32MB", "?"}

func ioHistAdd(req int64, ns int64) {
	for k := 7; k >= 0; k-- {
		if req >= ioHistBnd[k] {
			ioHistCnt[k]++
			ioHistNs[k] += ns
			return
		}
	}
	ioHistCnt[8]++
	ioHistNs[8] += ns
}

func ioStatsReset() {
	ioStatCalls, ioStatBytesReq, ioStatBytesGot = 0, 0, 0
	ioStatShort, ioStatErr, ioStatIONs = 0, 0, 0
	ioStatBigCalls, ioStatSmallCalls = 0, 0
	ioStatBigNs, ioStatSmallNs = 0, 0
	ioStatBigBytesReq, ioStatSmallBytesR = 0, 0
	ioHistCnt = [9]int64{}
	ioHistNs  = [9]int64{}
	ioOutN    = 0
}

// ioStatsLine - resumo legível dos contadores.
func ioStatsLine() string {
	if ioStatCalls == 0 {
		return ""
	}
	out := fmt.Sprintf("[io] calls=%d  CURTAS=%d  erros=%d  tempoEmReadFile=%.2fs",
		ioStatCalls, ioStatShort, ioStatErr, float64(ioStatIONs)/1e9)
	if ioStatBigCalls > 0 {
		out += fmt.Sprintf("\n     GRANDES (>=1MB): calls=%d  %.2f MB/req  %.2fs  (%.1f MB/s)",
			ioStatBigCalls, float64(ioStatBigBytesReq)/float64(ioStatBigCalls)/1e6,
			float64(ioStatBigNs)/1e9, float64(ioStatBigBytesReq)/1e6/(float64(ioStatBigNs)/1e9))
	}
	if ioStatSmallCalls > 0 {
		out += fmt.Sprintf("\n     PEQUENAS (<1MB): calls=%d  %.1f KB/req  %.2fs  (%.2f ms/call)",
			ioStatSmallCalls, float64(ioStatSmallBytesR)/float64(ioStatSmallCalls)/1e3,
			float64(ioStatSmallNs)/1e9, float64(ioStatSmallNs)/1e6/float64(ioStatSmallCalls))
	}
	for k := 0; k < 8; k++ {
		if ioHistCnt[k] == 0 {
			continue
		}
		out += fmt.Sprintf("\n     HIST %-9s calls=%d  %6.2fs  (%5.1f%% do tempo)",
			ioHistName[k], ioHistCnt[k], float64(ioHistNs[k])/1e9,
			100*float64(ioHistNs[k])/float64(ioStatIONs))
	}
	for i := 0; i < ioOutN; i++ {
		out += fmt.Sprintf("\n     OUT[%02d] off=%14d tam=%10d (%6.2f MB) %8.2f ms",
			i, ioOutB[i][0], ioOutB[i][1], float64(ioOutB[i][1])/1e6, float64(ioOutB[i][2])/1e6)
	}
	return out
}

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")

	procCreateFileW                  = kernel32.NewProc("CreateFileW")
	procReadFile                     = kernel32.NewProc("ReadFile")
	procCloseHandle                  = kernel32.NewProc("CloseHandle")
	procSetFilePointerEx             = kernel32.NewProc("SetFilePointerEx")
	procGetFileInformationByHandleEx = kernel32.NewProc("GetFileInformationByHandleEx")
	procGetVolumeInformationW        = kernel32.NewProc("GetVolumeInformationW")
	procSetPriorityClass             = kernel32.NewProc("SetPriorityClass")
	procGetCurrentProcess            = kernel32.NewProc("GetCurrentProcess")
	procOpenProcessToken             = advapi32.NewProc("OpenProcessToken")
	procGetTokenInformation          = advapi32.NewProc("GetTokenInformation")
	procShellExecuteW                = shell32.NewProc("ShellExecuteW")
	procVirtualAlloc                 = kernel32.NewProc("VirtualAlloc")
	procVirtualFree                  = kernel32.NewProc("VirtualFree")
	procGetProcessMemoryInfo         = kernel32.NewProc("K32GetProcessMemoryInfo")
	procGlobalMemoryStatusEx         = kernel32.NewProc("GlobalMemoryStatusEx")
	procDeviceIoControl              = kernel32.NewProc("DeviceIoControl")
	procGetFileSizeEx                = kernel32.NewProc("GetFileSizeEx")
)

// deviceIoControl - wrapper de DeviceIoControl (usado pelo USN Journal / F4).
func deviceIoControl(h syscall.Handle, code uint32, in unsafe.Pointer, inLen uint32,
	out unsafe.Pointer, outLen uint32) (uint32, error) {
	var br uint32
	r, _, e := procDeviceIoControl.Call(uintptr(h), uintptr(code),
		uintptr(in), uintptr(inLen), uintptr(out), uintptr(outLen),
		uintptr(unsafe.Pointer(&br)), 0)
	if r == 0 {
		return br, e
	}
	return br, nil
}

// memStatusEx - espelho de MEMORYSTATUSEX (winnt.h).
type memStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// availableRAM - RAM fisica livre em bytes (0 se indisponivel).
func availableRAM() uint64 {
	var m memStatusEx
	m.length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0
	}
	return m.availPhys
}

// pmc - espelho de PROCESS_MEMORY_COUNTERS (winnt.h), 64-bit.
type pmc struct {
	cb                 uint32
	pageFaultCount     uint32
	peakWorkingSetSize uintptr
	workingSetSize     uintptr
	quotaPeakPagedPool uintptr
	quotaPagedPool     uintptr
	quotaPeakNonPaged  uintptr
	quotaNonPaged      uintptr
	pagefileUsage      uintptr
	peakPagefileUsage  uintptr
}

// peakRSS - pico de working set do processo em bytes (0 se indisponivel).
func peakRSS() uint64 {
	var m pmc
	m.cb = uint32(unsafe.Sizeof(m))
	p, _, _ := procGetCurrentProcess.Call()
	r, _, _ := procGetProcessMemoryInfo.Call(p, uintptr(unsafe.Pointer(&m)), unsafe.Sizeof(m))
	if r == 0 {
		return 0
	}
	return uint64(m.peakWorkingSetSize)
}

const (
	fileListDirectory        = 0x0001
	fileReadAttributes       = 0x0080
	genericRead              = 0x80000000
	fileShareRead            = 0x00000001
	fileShareWrite           = 0x00000002
	openExisting             = 3
	fileFlagBackupSemantics  = 0x02000000
	fileFlagSequentialScan   = 0x08000000
	fileFlagNoBuffering      = 0x20000000
	fileAttributeDirectory   = 0x00000010
	fileAttributeReparse     = 0x00000400
	invalidHandleValue       = ^uintptr(0)
	errNoMoreFiles           = syscall.Errno(18)
	idlePriorityClass        = 0x00000040
	normalPriorityClass      = 0x00000020
	FileAttributeTagInfo     = 9
	FileIDBothDirInfo        = 10
	FileIDBothDirRestartInfo = 11
	FileIDExtdDirInfo        = 19 // FILE_ID_EXTD_DIR_INFO (traz ReparsePointTag)
	tokenQuery               = 0x0008
	tokenElevation           = 20
	swShowNormal             = 1

	memCommitReserve = 0x3000
	memRelease       = 0x8000
	pageReadWrite    = 0x04
)

// alignedAlloc - aloca memoria alinhada a PAGINA (exigido por NO_BUFFERING).
// Cai para make() se o VirtualAlloc falhar (nesse caso o chamador nao usa nobuf).
func alignedAlloc(n int) []byte {
	if n <= 0 {
		return nil
	}
	addr, _, _ := procVirtualAlloc.Call(0, uintptr(n), memCommitReserve, pageReadWrite)
	if addr == 0 {
		return make([]byte, n)
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), n)
}

func alignedFree(b []byte) {
	if len(b) > 0 {
		procVirtualFree.Call(uintptr(unsafe.Pointer(&b[0])), 0, memRelease)
	}
}

// sectorAlign - alinhamento exigido por NO_BUFFERING (setor fisico tipico).
const sectorAlign = 4096

// isNameSurrogate - mesmo critério do Go stdlib (os.ReadDir) no Windows:
// apenas reparse points "name surrogate" (junction/symlink, bit 0x20000000)
// são tratados como link; cloud (OneDrive), WOF, WCI, dedup NÃO são.
func isNameSurrogate(tag uint32) bool {
	return tag&0x20000000 != 0
}

// tags de reparse (referência)
const (
	ioReparseTagMountPoint = 0xA0000003 // junction
	ioReparseTagSymlink    = 0xA000000C // symbolic link
)

// longPath prefixa \\?\ quando necessario (limite MAX_PATH 260).
func longPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`) {
		return p
	}
	if len(p) >= 248 {
		return `\\?\` + p
	}
	return p
}

func openFile(path string, access uint32, share uint32, flags uint32) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(longPath(path))
	if err != nil {
		return 0, err
	}
	h, _, e := procCreateFileW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(access),
		uintptr(share),
		0,
		openExisting,
		uintptr(flags),
		0,
	)
	if h == invalidHandleValue {
		return 0, e
	}
	return syscall.Handle(h), nil
}

func openDir(path string) (syscall.Handle, error) {
	return openFile(path, fileListDirectory|fileReadAttributes, fileShareRead|fileShareWrite|4, fileFlagBackupSemantics)
}

func openVolume(drive string, nobuf bool) (syscall.Handle, error) {
	// drive = "C:" -> \\.\C:
	flags := uint32(fileFlagSequentialScan)
	if nobuf {
		flags |= fileFlagNoBuffering
	}
	return openFile(`\\.\`+drive, genericRead, fileShareRead|fileShareWrite, flags)
}

func closeHandle(h syscall.Handle) {
	if h != 0 && uintptr(h) != invalidHandleValue {
		procCloseHandle.Call(uintptr(h))
	}
}

// getDirEntries - le um lote de entradas do diretorio (FILE_ID_EXTD_DIR_INFO,
// que inclui ReparsePointTag para distinguir junction/symlink de cloud/WOF/dedup).
// Retorna (0, nil) quando acabou (ERROR_NO_MORE_FILES).
func getDirEntries(h syscall.Handle, buf []byte) (int, error) {
	r1, _, e := procGetFileInformationByHandleEx.Call(
		uintptr(h),
		FileIDExtdDirInfo,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if r1 != 0 {
		return len(buf), nil
	}
	if e == errNoMoreFiles {
		return 0, nil
	}
	return 0, e
}

// fsNameOf - nome do filesystem do volume (ex.: "NTFS").
func fsNameOf(drive string) string {
	p, err := syscall.UTF16PtrFromString(drive + `\`)
	if err != nil {
		return ""
	}
	buf := make([]uint16, 32)
	r, _, _ := procGetVolumeInformationW.Call(
		uintptr(unsafe.Pointer(p)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

func setFilePointer(h syscall.Handle, off int64) error {
	r1, _, e := procSetFilePointerEx.Call(uintptr(h), uintptr(off), 0, 0) // FILE_BEGIN = 0
	if r1 == 0 {
		return e
	}
	return nil
}

// overlapped - espelho de OVERLAPPED (winnt.h). Usado apenas para carregar o
// offset da leitura: com um handle SINCRONO, passar lpOverlapped != NULL faz o
// ReadFile usar esse offset e NAO mexer no file pointer do handle.
type overlapped struct {
	internal     uintptr
	internalHigh uintptr
	offset       uint32
	offsetHigh   uint32
	hEvent       uintptr
}

// readVolumeAt - leitura POSICIONADA do volume cru.
//
// Por que existe: o motor paralelo (mft_par.go) dispara N leituras concorrentes
// no MESMO handle. O caminho antigo (SetFilePointerEx + ReadFile) usa um file
// pointer unico e portanto SERIALIZA tudo. Aqui o offset vai no OVERLAPPED, o
// que permite leituras concorrentes sem estado compartilhado.
func readVolumeAt(h syscall.Handle, off int64, buf []byte) error {
	if len(buf) == 0 {
		return nil
	}
	total := 0
	for total < len(buf) {
		cur := off + int64(total)
		var ov overlapped
		ov.offset = uint32(cur)
		ov.offsetHigh = uint32(uint64(cur) >> 32)
		var n uint32
		_ioT0 := time.Now()
		r1, _, e := procReadFile.Call(
			uintptr(h),
			uintptr(unsafe.Pointer(&buf[total])),
			uintptr(len(buf)-total),
			uintptr(unsafe.Pointer(&n)),
			uintptr(unsafe.Pointer(&ov)),
		)
		_ioElapsed := time.Since(_ioT0).Nanoseconds()
		atomic.AddInt64(&ioStatIONs, _ioElapsed)
		atomic.AddInt64(&ioStatCalls, 1)
		atomic.AddInt64(&ioStatBytesReq, int64(len(buf)-total))
		// separa o stream grande das leituras de metadados que se intercalam
		if req := len(buf) - total; req >= 1<<20 {
			atomic.AddInt64(&ioStatBigCalls, 1)
			atomic.AddInt64(&ioStatBigBytesReq, int64(req))
			atomic.AddInt64(&ioStatBigNs, _ioElapsed)
		} else {
			atomic.AddInt64(&ioStatSmallCalls, 1)
			atomic.AddInt64(&ioStatSmallBytesR, int64(req))
			atomic.AddInt64(&ioStatSmallNs, _ioElapsed)
		}
		ioHistAdd(int64(len(buf)-total), _ioElapsed)
		// log dos primeiros pedidos FORA do stream (offset+tamanho): identifica
		// qual caminho do motor os emite.
		if req := len(buf) - total; req < 16<<20 && ioOutN < len(ioOutB) {
			ioOutB[ioOutN] = [3]int64{cur, int64(req), _ioElapsed}
			ioOutN++
		}
		if uint32(n) < uint32(len(buf)-total) {
			atomic.AddInt64(&ioStatShort, 1)
		}
		if r1 == 0 {
			if n > 0 {
				atomic.AddInt64(&ioStatErr, 1)
				total += int(n)
				continue
			}
			if total >= len(buf) {
				return nil
			}
			return e
		}
		if n == 0 {
			atomic.AddInt64(&ioStatErr, 1)
			return fmt.Errorf("leitura curta: %d/%d bytes @%d", total, len(buf), off)
		}
		atomic.AddInt64(&ioStatBytesGot, int64(n))
		total += int(n)
	}
	return nil
}

func readAt(h syscall.Handle, buf []byte) (int, error) {
	var n uint32
	r1, _, e := procReadFile.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&n)),
		0,
	)
	if r1 == 0 {
		return int(n), e
	}
	return int(n), nil
}

// readAtOff - posiciona e lê exatamente len(buf) bytes (com loop p/ curtas).
func readAtOff(h syscall.Handle, off int64, buf []byte) error {
	if len(buf) == 0 {
		return nil
	}
	if err := setFilePointer(h, off); err != nil {
		return err
	}
	total := 0
	for total < len(buf) {
		n, err := readAt(h, buf[total:])
		if n > 0 {
			total += n
		}
		if err != nil {
			if total >= len(buf) {
				return nil
			}
			return err
		}
		if n == 0 {
			return fmt.Errorf("leitura curta: %d/%d bytes @%d", total, len(buf), off)
		}
	}
	return nil
}

func setPriority(idle bool) {
	p, _, _ := procGetCurrentProcess.Call()
	class := uintptr(normalPriorityClass)
	if idle {
		class = idlePriorityClass
	}
	procSetPriorityClass.Call(p, class)
}

// isElevated - checa se o processo roda elevado (token TokenElevation).
func isElevated() bool {
	cur, _, _ := procGetCurrentProcess.Call()
	var tok syscall.Handle
	r1, _, _ := procOpenProcessToken.Call(cur, tokenQuery, uintptr(unsafe.Pointer(&tok)))
	if r1 == 0 {
		return false
	}
	defer procCloseHandle.Call(uintptr(tok))
	var te struct{ IsElevated uint32 }
	var retLen uint32
	r2, _, _ := procGetTokenInformation.Call(
		uintptr(tok),
		tokenElevation,
		uintptr(unsafe.Pointer(&te)),
		unsafe.Sizeof(te),
		uintptr(unsafe.Pointer(&retLen)),
	)
	return r2 != 0 && te.IsElevated != 0
}

// elevateSelf - relanca o proprio binario elevado (UAC). Bloqueante no dialogo.
func elevateSelf(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	op, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	params, _ := syscall.UTF16PtrFromString(quoteArgs(args))
	r, _, e := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(op)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(params)),
		0,
		swShowNormal,
	)
	if r <= 32 {
		return fmt.Errorf("ShellExecuteW falhou (codigo %d): %v", r, e)
	}
	return nil
}

func quoteArgs(args []string) string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if strings.ContainsAny(a, " \t") {
			out = append(out, `"`+strings.ReplaceAll(a, `"`, `\"`)+`"`)
		} else {
			out = append(out, a)
		}
	}
	return strings.Join(out, " ")
}
