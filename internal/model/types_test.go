package model

import "testing"

func TestRegistrySource_String(t *testing.T) {
	tests := []struct {
		name string
		src  RegistrySource
		want string
	}{
		{"dockerhub", SourceDockerHub, "dockerhub"},
		{"ghcr", SourceGHCR, "ghcr"},
		{"unknown is empty", SourceUnknown, ""},
		{"out-of-range is empty", RegistrySource(99), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.src.String(); got != tt.want {
				t.Errorf("RegistrySource(%d).String() = %q, want %q", tt.src, got, tt.want)
			}
		})
	}
}
