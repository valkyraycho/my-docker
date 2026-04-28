package volume

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    *Spec
		wantErr string // substring expected in error, or "" for no error
	}{
		// --- Happy paths ---
		{
			name:  "bind mount default rw",
			input: "/host/data:/app/data",
			want:  &Spec{Kind: Bind, Source: "/host/data", Target: "/app/data", ReadOnly: false},
		},
		{
			name:  "bind mount explicit rw",
			input: "/host/data:/app/data:rw",
			want:  &Spec{Kind: Bind, Source: "/host/data", Target: "/app/data", ReadOnly: false},
		},
		{
			name:  "bind mount read-only",
			input: "/host/data:/app/data:ro",
			want:  &Spec{Kind: Bind, Source: "/host/data", Target: "/app/data", ReadOnly: true},
		},
		{
			name:  "named volume default rw",
			input: "pgdata:/var/lib/postgres",
			want:  &Spec{Kind: Named, Source: "pgdata", Target: "/var/lib/postgres", ReadOnly: false},
		},
		{
			name:  "named volume read-only",
			input: "configs:/etc/configs:ro",
			want:  &Spec{Kind: Named, Source: "configs", Target: "/etc/configs", ReadOnly: true},
		},
		{
			name:  "named volume with deep target path",
			input: "logs:/var/log/myapp/nested",
			want:  &Spec{Kind: Named, Source: "logs", Target: "/var/log/myapp/nested", ReadOnly: false},
		},

		// --- Error cases ---
		{
			name:    "no colon",
			input:   "onlyonepart",
			wantErr: "expected src:dst",
		},
		{
			name:    "too many colons",
			input:   "a:b:c:d",
			wantErr: "expected src:dst",
		},
		{
			name:    "empty source",
			input:   ":/app/data",
			wantErr: "source is empty",
		},
		{
			name:    "empty target",
			input:   "/host/data:",
			wantErr: "target is empty",
		},
		{
			name:    "relative target",
			input:   "/host/data:app/data",
			wantErr: "must be absolute",
		},
		{
			name:    "invalid mode",
			input:   "/host/data:/app/data:readwrite",
			wantErr: "must be 'ro' or 'rw'",
		},
		{
			name:    "named volume with slash",
			input:   "my/volume:/app/data",
			wantErr: "must not contain slashes",
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: "expected src:dst",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.input)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse(%q): want error containing %q, got nil", tt.input, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse(%q): error = %q, want substring %q", tt.input, err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("Parse(%q): unexpected error: %v", tt.input, err)
			}
			if got == nil {
				t.Fatalf("Parse(%q): got nil spec, want %+v", tt.input, tt.want)
			}
			if *got != *tt.want {
				t.Fatalf("Parse(%q):\n  got:  %+v\n  want: %+v", tt.input, *got, *tt.want)
			}
		})
	}
}
