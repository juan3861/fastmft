package main

import (
	"testing"
)

func TestMftBitmap_Basic(t *testing.T) {
	// 100 registros (13 bytes)
	data := make([]byte, 13)
	// Marcar registro 10 e 75 como ativos
	data[10>>3] |= 1 << (10 & 7)
	data[75>>3] |= 1 << (75 & 7)

	bm := newMftBitmap(data, 100)

	if !bm.isRecordInUse(10) {
		t.Errorf("registro 10 deveria estar em uso")
	}
	if !bm.isRecordInUse(75) {
		t.Errorf("registro 75 deveria estar em uso")
	}
	if bm.isRecordInUse(9) {
		t.Errorf("registro 9 nao deveria estar em uso")
	}
	if bm.isRecordInUse(11) {
		t.Errorf("registro 11 nao deveria estar em uso")
	}

	// Faixa sem nenhum registro ativo: 0..10
	if bm.chunkHasInUse(0, 10) {
		t.Errorf("faixa 0..10 nao deveria ter em uso")
	}
	// Faixa contendo registro 10: 0..11
	if !bm.chunkHasInUse(0, 11) {
		t.Errorf("faixa 0..11 deveria ter em uso")
	}
	// Faixa intermediaria: 11..75
	if bm.chunkHasInUse(11, 64) {
		t.Errorf("faixa 11..75 nao deveria ter em uso")
	}
	// Faixa contendo 75: 75..1
	if !bm.chunkHasInUse(75, 1) {
		t.Errorf("faixa 75..76 deveria ter em uso")
	}
}

func TestMftBitmap_LargeSpan(t *testing.T) {
	// 1 milhao de registros (125.000 bytes)
	data := make([]byte, 125000)
	// Apenas 1 registro ativo em 500.000
	target := 500000
	data[target>>3] |= 1 << (target & 7)

	bm := newMftBitmap(data, 1000000)

	// Blocos de 16384 registros (16 MB de MFT)
	chunkSize := 16384
	var activeChunks, emptyChunks int
	for start := 0; start < 1000000; start += chunkSize {
		if bm.chunkHasInUse(start, chunkSize) {
			activeChunks++
		} else {
			emptyChunks++
		}
	}

	if activeChunks != 1 {
		t.Errorf("esperava exatamente 1 chunk ativo, obteve %d", activeChunks)
	}
	if emptyChunks < 60 {
		t.Errorf("esperava mais de 60 chunks vazios, obteve %d", emptyChunks)
	}
}
