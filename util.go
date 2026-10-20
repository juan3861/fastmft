package main

// util.go - formatacao e helpers de caminho.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func fmtBytes(n int64) string {
	f := float64(n)
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.2f %s", f, units[i])
}

func fmtInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := ""
	if strings.HasPrefix(s, "-") {
		neg, s = "-", s[1:]
	}
	if len(s) <= 3 {
		return neg + s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return neg + strings.Join(parts, ".")
}

func joinPath(parent, name string) string {
	if strings.HasSuffix(parent, `\`) {
		return parent + name
	}
	return parent + `\` + name
}

func parentOf(p string) string {
	if len(p) <= 3 {
		return ""
	}
	if strings.HasSuffix(p, `\`) {
		p = p[:len(p)-1]
	}
	i := strings.LastIndexByte(p, '\\')
	if i <= 2 {
		return p[:3]
	}
	return p[:i]
}

func depthOf(p string) int { return strings.Count(p, `\`) }

// normalizeRoot - "C:" -> "C:\" ; uniformiza separadores para "\".
func normalizeRoot(t string) string {
	t = strings.TrimSpace(t)
	t = strings.ReplaceAll(t, "/", `\`)
	if len(t) == 2 && t[1] == ':' {
		return t + `\`
	}
	if strings.HasSuffix(t, `:\`) {
		return t
	}
	return strings.TrimSuffix(t, `\`)
}

func driveOf(p string) string {
	if len(p) >= 2 && p[1] == ':' {
		return strings.ToUpper(p[:2])
	}
	return ""
}

// isDriveRoot - true se o alvo e uma raiz de volume (C: ou C:\).
func isDriveRoot(t string) bool {
	t = strings.TrimSpace(t)
	if len(t) == 2 && t[1] == ':' {
		return true
	}
	return len(t) == 3 && t[1] == ':' && t[2] == '\\'
}

func humanDur(d time.Duration) string {
	s := d.Seconds()
	if s < 60 {
		return fmt.Sprintf("%.1fs", s)
	}
	m := int(s) / 60
	sec := int(s) % 60
	if m < 60 {
		return fmt.Sprintf("%dm%02ds", m, sec)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}

// utf16Bytes - converte bytes UTF-16LE (sem terminador) para string Go.
func utf16Bytes(b []byte) string {
	n := len(b) / 2
	if n == 0 {
		return ""
	}
	var stack [512]byte
	if n <= len(stack) {
		ascii := true
		for i := 0; i < n; i++ {
			lo := b[2*i]
			hi := b[2*i+1]
			if hi != 0 || lo >= 0x80 {
				ascii = false
				break
			}
			stack[i] = lo
		}
		if ascii {
			return string(stack[:n])
		}
	}
	var sb strings.Builder
	sb.Grow(n)
	for i := 0; i < n; i++ {
		r := rune(uint16(b[2*i]) | uint16(b[2*i+1])<<8)
		if r >= 0xD800 && r <= 0xDBFF && i+1 < n {
			r2 := rune(uint16(b[2*(i+1)]) | uint16(b[2*(i+1)+1])<<8)
			if r2 >= 0xDC00 && r2 <= 0xDFFF {
				r = ((r - 0xD800) << 10) + (r2 - 0xDC00) + 0x10000
				i++
			}
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// joinPathUTF16 - concatena parent + `\` + nome UTF-16LE em uma unica alocacao no caso ASCII comum.
func joinPathUTF16(parent string, b []byte) string {
	n := len(b) / 2
	if n == 0 {
		return parent
	}
	plen := len(parent)
	sep := 1
	if plen > 0 && parent[plen-1] == '\\' {
		sep = 0
	}
	var stack [1024]byte
	if plen+sep+n <= len(stack) {
		copy(stack[:plen], parent)
		if sep == 1 {
			stack[plen] = '\\'
		}
		outOff := plen + sep
		ascii := true
		for i := 0; i < n; i++ {
			lo := b[2*i]
			hi := b[2*i+1]
			if hi != 0 || lo >= 0x80 {
				ascii = false
				break
			}
			stack[outOff+i] = lo
		}
		if ascii {
			return string(stack[:outOff+n])
		}
	}
	return joinPath(parent, utf16Bytes(b))
}
