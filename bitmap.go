package main

import (
	"encoding/binary"
)

// MftBitmap gerencia o mapa de bits de alocação de registros do $MFT.
// Cada bit representa um registro (bit i = 1 se o registro i está em uso).
// Permite que o leitor de disco pule blocos físicos inteiros que contenham
// apenas registros vagos/deletados, economizando até 40% de I/O de disco.
type MftBitmap struct {
	data        []byte
	recordCount int
}

func newMftBitmap(data []byte, recCount int) *MftBitmap {
	return &MftBitmap{
		data:        data,
		recordCount: recCount,
	}
}

// isRecordInUse verifica se o registro de índice idx está ativo (1) ou livre (0).
func (bm *MftBitmap) isRecordInUse(idx int) bool {
	if bm == nil || idx < 0 || idx >= bm.recordCount {
		return true // conservador: se fora do range ou sem bitmap, assume em uso
	}
	byteIdx := idx >> 3
	if byteIdx >= len(bm.data) {
		return true
	}
	return (bm.data[byteIdx] & (1 << (idx & 7))) != 0
}

// chunkHasInUse verifica se no intervalo [startIdx, startIdx+count) existe
// ALGUM registro alocado (bit 1). Otimizado com varredura uint64 (8 bytes = 64 registros por ciclo).
func (bm *MftBitmap) chunkHasInUse(startIdx, count int) bool {
	if bm == nil || count <= 0 {
		return true
	}
	endIdx := startIdx + count
	if startIdx < 0 {
		startIdx = 0
	}
	if endIdx > bm.recordCount {
		endIdx = bm.recordCount
	}
	if startIdx >= endIdx {
		return false
	}

	startByte := startIdx >> 3
	endByte := (endIdx + 7) >> 3
	if startByte >= len(bm.data) {
		return false
	}
	if endByte > len(bm.data) {
		endByte = len(bm.data)
	}

	b := bm.data[startByte:endByte]

	// Se começar desalinhado no byte inicial:
	if startIdx&7 != 0 && len(b) > 0 {
		mask := byte(0xFF << (startIdx & 7))
		if len(b) == 1 && endIdx&7 != 0 {
			mask &= byte((1 << (endIdx & 7)) - 1)
		}
		if (b[0] & mask) != 0 {
			return true
		}
		b = b[1:]
		if len(b) == 0 {
			return false
		}
	}

	// Se terminar desalinhado no byte final:
	if endIdx&7 != 0 && len(b) > 0 {
		tailMask := byte((1 << (endIdx & 7)) - 1)
		tail := b[len(b)-1]
		if (tail & tailMask) != 0 {
			return true
		}
		b = b[:len(b)-1]
	}

	// Varredura ultra-rápida em blocos de 8 bytes (64 bits):
	for len(b) >= 8 {
		if binary.LittleEndian.Uint64(b) != 0 {
			return true
		}
		b = b[8:]
	}
	// Bytes restantes
	for _, v := range b {
		if v != 0 {
			return true
		}
	}
	return false
}
