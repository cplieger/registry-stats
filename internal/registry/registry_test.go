package registry

import "testing"

func TestID_String(t *testing.T) {
	tests := []struct {
		name string
		src  ID
		want string
	}{
		{"dockerhub", DockerHub, "dockerhub"},
		{"ghcr", GHCR, "ghcr"},
		{"unknown is empty", Unknown, ""},
		{"out-of-range is empty", ID(99), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.src.String(); got != tt.want {
				t.Errorf("ID(%d).String() = %q, want %q", tt.src, got, tt.want)
			}
		})
	}
}
