package imessage

import (
	"encoding/hex"
	"testing"
)

func TestExtractTextFromAttributedBody(t *testing.T) {
	tests := []struct {
		name string
		hex  string
		want string
	}{
		{
			name: "Stop (short NSString)",
			hex:  "040B73747265616D747970656481E803840140848484124E5341747472696275746564537472696E67008484084E534F626A656374008592848484084E53537472696E67019484012B0453746F7086840269490104928484840C4E5344696374696F6E617279009484016901928496961D5F5F6B494D4D657373616765506172744174747269627574654E616D658692848484084E534E756D626572008484074E5356616C7565009484012A84999900868686",
			want: "Stop",
		},
		{
			name: "Coming! (short NSString)",
			hex:  "040B73747265616D747970656481E803840140848484124E5341747472696275746564537472696E67008484084E534F626A656374008592848484084E53537472696E67019484012B07436F6D696E672186840269490107928484840C4E5344696374696F6E617279009484016901928496961D5F5F6B494D4D657373616765506172744174747269627574654E616D658692848484084E534E756D626572008484074E5356616C7565009484012A84999900868686",
			want: "Coming!",
		},
		{
			name: "Sooooo good (with trailing space)",
			hex:  "040B73747265616D747970656481E803840140848484124E5341747472696275746564537472696E67008484084E534F626A656374008592848484084E53537472696E67019484012B0C536F6F6F6F6F20676F6F64208684026949010C928484840C4E5344696374696F6E617279009484016901928496961D5F5F6B494D4D657373616765506172744174747269627574654E616D658692848484084E534E756D626572008484074E5356616C7565009484012A84999900868686",
			want: "Sooooo good",
		},
		{
			name: "Loved with smart quotes (UTF-8)",
			hex:  "040B73747265616D747970656481E803840140848484124E5341747472696275746564537472696E67008484084E534F626A656374008592848484084E53537472696E67019484012B134C6F76656420E2809C436F6D696E6721E2809D8684026949010F928484840C4E5344696374696F6E617279009484016901928496961D5F5F6B494D4D657373616765506172744174747269627574654E616D658692848484084E534E756D626572008484074E5356616C7565009484012A84999900868686",
			want: "Loved \u201cComing!\u201d",
		},
		{
			name: "Long message with multi-byte length (>128 bytes)",
			hex:  "040B73747265616D747970656481E803840140848484124E5341747472696275746564537472696E67008484084E534F626A656374008592848484084E53537472696E67019484012B819F004E6F7420737572652069662049E280996C6C206D616B652069742C2062757420696620796F75206368617420776974682068696D20796F752063616E2074656C6C2068696D2074686174204920706572736F6E616C6C7920636F6E7472696275746564206174206C65617374203130302070616765766965777320746F20686973204F70656E436C617720646F636B65722054494C20706F737420F09F9885868402694901819B00928484840C4E5344696374696F6E617279009484016901928496961D5F5F6B494D4D657373616765506172744174747269627574654E616D658692848484084E534E756D626572008484074E5356616C7565009484012A84999900868686",
			want: "Not sure if I\u2019ll make it, but if you chat with him you can tell him that I personally contributed at least 100 pageviews to his OpenClaw docker TIL post \U0001f605",
		},
		{
			name: "empty data",
			hex:  "",
			want: "",
		},
		{
			name: "too short",
			hex:  "040B73747265616D",
			want: "",
		},
		{
			name: "no NSString marker",
			hex:  "040B73747265616D747970656481E803840140848484124E5341747472696275746564537472696E67008484084E534F626A656374008592",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := hex.DecodeString(tt.hex)
			if err != nil {
				t.Fatalf("invalid hex: %v", err)
			}
			got := extractTextFromAttributedBody(data)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestScanForStringMarkerPicksLongest(t *testing.T) {
	// Simulate a false-positive 0x2B marker in binary data before the real
	// string marker. The first 0x2B points to "op" (a suffix of the real
	// text), while the second points to the full "Stop". The function should
	// return the longest candidate.
	data := []byte{
		0x2B, 0x02, 'o', 'p', // false positive: "op"
		0x2B, 0x04, 'S', 't', 'o', 'p', // real marker: "Stop"
	}
	got := scanForStringMarker(data, 0, 0x2B)
	if got != "Stop" {
		t.Errorf("got %q, want %q", got, "Stop")
	}
}

func TestIsOnlyObjectReplacement(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"\uFFFC", true},
		{"\uFFFC\uFFFC", true},
		{"", false},
		{"hello", false},
		{"\uFFFChello", false},
	}
	for _, tt := range tests {
		if got := isOnlyObjectReplacement(tt.input); got != tt.want {
			t.Errorf("isOnlyObjectReplacement(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestStripObjectReplacement(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"\uFFFCHello world", "Hello world"},
		{"Hello\uFFFC world", "Hello world"},
		{"\uFFFC", ""},
		{"No replacement", "No replacement"},
	}
	for _, tt := range tests {
		if got := stripObjectReplacement(tt.input); got != tt.want {
			t.Errorf("stripObjectReplacement(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
