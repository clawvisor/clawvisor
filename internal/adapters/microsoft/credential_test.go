package microsoft

import (
	"testing"
)

func TestExtractToken(t *testing.T) {
	tests := []struct {
		name      string
		credBytes []byte
		want      string
		wantErr   bool
	}{
		{
			name:      "valid token",
			credBytes: []byte(`{"token": "token123"}`),
			want:      "token123",
			wantErr:   false,
		},
		{
			name:      "valid access_token",
			credBytes: []byte(`{"access_token": "token123"}`),
			want:      "token123",
			wantErr:   false,
		},
		{
			name:      "missing token",
			credBytes: []byte(`{"other": "value"}`),
			want:      "",
			wantErr:   true,
		},
		{
			name:      "invalid json",
			credBytes: []byte(`{invalid}`),
			want:      "",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractToken(tt.credBytes)
			if (err != nil) != tt.wantErr {
				t.Errorf("ExtractToken() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ExtractToken() got = %v, want %v", got, tt.want)
			}
		})
	}
}
