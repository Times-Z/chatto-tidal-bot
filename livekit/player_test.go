//go:build cgo

package livekit

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestBytesToPCM16(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want []int16
	}{
		{
			name: "two samples",
			data: func() []byte {
				b := make([]byte, 4)
				binary.LittleEndian.PutUint16(b[0:2], 1000)
				binary.LittleEndian.PutUint16(b[2:4], 2000)
				return b
			}(),
			want: []int16{1000, 2000},
		},
		{
			name: "odd byte stripped",
			data: func() []byte {
				b := make([]byte, 3)
				binary.LittleEndian.PutUint16(b[0:2], 42)
				b[2] = 0xFF
				return b
			}(),
			want: []int16{42},
		},
		{
			name: "empty input",
			data: []byte{},
			want: []int16{},
		},
		{
			name: "one sample",
			data: func() []byte {
				b := make([]byte, 2)
				binary.LittleEndian.PutUint16(b, math.MaxUint16)
				return b
			}(),
			want: []int16{-1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := bytesToPCM16(tc.data)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("samples[%d] = %d, want %d", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSetVolume(t *testing.T) {
	p := &Player{volume: 1.0}

	p.SetVolume(0.5)
	if p.volume != 0.5 {
		t.Fatalf("expected 0.5, got %f", p.volume)
	}

	p.SetVolume(-0.1)
	if p.volume != 0 {
		t.Fatalf("expected 0 for negative, got %f", p.volume)
	}

	p.SetVolume(3.0)
	if p.volume != 2.0 {
		t.Fatalf("expected 2.0 for over max, got %f", p.volume)
	}

	p.SetVolume(1.0)
	if p.volume != 1.0 {
		t.Fatalf("expected 1.0, got %f", p.volume)
	}
}

func TestDefaultSampleRate(t *testing.T) {
	if _, err := NewPlayer(Config{
		URL: "wss://invalid.example.com", Token: "tok", Room: "room1",
	}); err == nil {
		t.Fatal("expected connection error, not nil")
	}
}
